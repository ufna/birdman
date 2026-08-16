// ServerLink implementation: the liba side of the liba<->agent contract
// (docs/specs/protocol.md §2). One background I/O thread per link handles
// connect/reconnect with backoff, NDJSON reads/writes and timers; public
// methods only mutate state under a mutex and wake the thread via a
// self-pipe. Reference behaviour: examples/stub-server (game side) and
// agent/internal/uds/server.go (agent side).

#include "birdman/birdman.h"

#include <fcntl.h>
#include <poll.h>
#include <sys/socket.h>
#include <sys/un.h>
#include <unistd.h>

#include <algorithm>
#include <atomic>
#include <cerrno>
#include <chrono>
#include <cstdarg>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <deque>
#include <map>
#include <mutex>
#include <string_view>
#include <thread>
#include <utility>
#include <vector>

#include "json.h"

namespace birdman {

namespace {

using Clock = std::chrono::steady_clock;

constexpr auto kBackoffMin = std::chrono::milliseconds(100);
constexpr auto kBackoffMax = std::chrono::milliseconds(2000);
constexpr auto kWriteStall = std::chrono::seconds(3);      // agent uses the same write timeout
constexpr auto kPlayersEvery = std::chrono::seconds(10);   // protocol: players >=1/10s
constexpr auto kMetricEvery = std::chrono::seconds(1);     // protocol: metric <=1/s per name
constexpr auto kShutdownFlush = std::chrono::seconds(1);
constexpr size_t kRingCap = 256;      // outgoing event frames while disconnected (sdk.md §2)
constexpr size_t kEventCap = 64;      // queued allocated/drain events in kPoll mode
constexpr size_t kMaxLine = 256 * 1024;  // agent's maxLine

const char* EnvOr(const char* name, const char* def) {
  const char* v = std::getenv(name);
  return (v != nullptr && v[0] != '\0') ? v : def;
}

std::string PlayersFrame(int count) {
  return json::Frame("players", json::Obj().Int("count", count).Done());
}

std::string MetricFrame(const std::string& name, double value) {
  return json::Frame("metric", json::Obj().Str("name", name).Num("value", value).Done());
}

}  // namespace

std::string SdkVersion() { return "birdman-cpp/" BIRDMAN_SDK_VERSION; }

class ServerLink::Impl {
 public:
  ~Impl() { ShutdownImpl(); }

  bool InitImpl(const Config& config) {
    std::lock_guard<std::mutex> lock(mu_);
    if (inited_ || shut_) return managed_.load();
    inited_ = true;
    cfg_ = config;
    socket_path_ = !cfg_.socket_path.empty() ? cfg_.socket_path : EnvOr("BIRDMAN_SOCKET", "");
    server_id_ = !cfg_.server_id.empty() ? cfg_.server_id : EnvOr("BIRDMAN_SERVER_ID", "");
    port_ = cfg_.port != 0 ? cfg_.port : std::atoi(EnvOr("BIRDMAN_PORT", "0"));
    debug_ = std::getenv("BIRDMAN_SDK_DEBUG") != nullptr;
    if (socket_path_.empty()) {
      // No-op mode: no BIRDMAN_SOCKET -> no thread, no sockets, ever.
      return false;
    }
    int fds[2] = {-1, -1};
    if (pipe(fds) != 0) {
      Debug("pipe: %s (falling back to no-op)", std::strerror(errno));
      return false;
    }
    for (int fd : fds) {
      fcntl(fd, F_SETFL, O_NONBLOCK);
      fcntl(fd, F_SETFD, FD_CLOEXEC);
    }
    wake_r_ = fds[0];
    wake_w_ = fds[1];
    managed_.store(true);
    io_ = std::thread([this] { IoLoop(); });
    return true;
  }

  bool IsManaged() const { return managed_.load(); }

  void NotifyReady() {
    std::lock_guard<std::mutex> lock(mu_);
    if (!Active()) return;
    ready_ = true;
    // State frame: replay-on-reconnect covers the disconnected case, so it
    // is never ring-buffered (avoids duplicates next to the replay).
    if (connected_) {
      PushRing(json::Frame("ready", "{}"));
      WakeLocked();
    }
  }

  void NotifyMatchStart() {
    std::lock_guard<std::mutex> lock(mu_);
    if (!Active()) return;
    PushRing(json::Frame("match_start", json::Obj().Str("match_id", match_id_).Done()));
    WakeLocked();
  }

  void NotifyMatchEnd(MatchResult result) {
    std::lock_guard<std::mutex> lock(mu_);
    if (!Active()) return;
    const char* r = result == MatchResult::kCompleted ? "completed" : "aborted";
    PushRing(json::Frame("match_end",
                         json::Obj().Str("match_id", match_id_).Str("result", r).Done()));
    // One-shot server contract: after match_end the process exits, so stop
    // advertising ready on future reconnects (a replayed `ready` could get
    // this dying server allocated again).
    ready_ = false;
    WakeLocked();
  }

  void SetPlayerCount(int count) {
    std::lock_guard<std::mutex> lock(mu_);
    if (!Active()) return;
    players_ = count;
    players_known_ = true;
    if (connected_) {
      PushRing(PlayersFrame(count));
      WakeLocked();
    }
  }

  void ReportMetric(const std::string& name, double value) {
    std::lock_guard<std::mutex> lock(mu_);
    if (!Active()) return;
    MetricSlot& slot = metrics_[name];
    slot.value = value;
    const auto now = Clock::now();
    if (connected_ && now - slot.last_send >= kMetricEvery) {
      slot.last_send = now;
      slot.dirty = false;
      PushRing(MetricFrame(name, value));
      WakeLocked();
    } else if (!slot.dirty) {
      // Coalesced; flushed by the I/O timer (or on reconnect). Wake the I/O
      // thread so it schedules the flush instead of sleeping past it.
      slot.dirty = true;
      if (connected_) WakeLocked();
    }
  }

  int PollCallbacks() {
    std::vector<PendingEvent> batch;
    {
      std::lock_guard<std::mutex> lock(mu_);
      if (events_.empty()) return 0;
      batch.assign(events_.begin(), events_.end());
      events_.clear();
    }
    for (const PendingEvent& ev : batch) {  // invoked without the lock
      if (ev.is_drain) {
        if (cfg_.on_drain_requested) cfg_.on_drain_requested(ev.drain);
      } else {
        if (cfg_.on_allocated) cfg_.on_allocated(ev.allocated);
      }
    }
    return static_cast<int>(batch.size());
  }

  void ShutdownImpl() {
    {
      std::lock_guard<std::mutex> lock(mu_);
      if (shut_) return;
      shut_ = true;
    }
    stop_.store(true);
    Wake();
    if (io_.joinable()) {
      if (io_.get_id() == std::this_thread::get_id()) {
        io_.detach();  // Shutdown() from inside a kDispatch callback
      } else {
        io_.join();
      }
    }
    if (wake_r_ >= 0) close(wake_r_);
    if (wake_w_ >= 0) close(wake_w_);
    wake_r_ = wake_w_ = -1;
  }

  std::string ServerIdImpl() const {
    std::lock_guard<std::mutex> lock(mu_);
    return server_id_;
  }

  int PortImpl() const {
    std::lock_guard<std::mutex> lock(mu_);
    return port_;
  }

  std::string MatchIdImpl() const {
    std::lock_guard<std::mutex> lock(mu_);
    return match_id_;
  }

 private:
  struct PendingEvent {
    bool is_drain = false;
    AllocatedEvent allocated;
    DrainEvent drain;
  };

  struct MetricSlot {
    double value = 0.0;
    bool dirty = false;
    Clock::time_point last_send{};  // epoch: first report always sends
  };

  bool Active() const { return managed_.load() && !shut_; }  // callers hold mu_

  void Debug(const char* fmt, ...) const {
    if (!debug_) return;
    va_list ap;
    va_start(ap, fmt);
    std::fprintf(stderr, "[birdman] ");
    std::vfprintf(stderr, fmt, ap);
    std::fprintf(stderr, "\n");
    va_end(ap);
  }

  // callers hold mu_
  void PushRing(std::string frame) {
    if (outq_.size() >= kRingCap) outq_.pop_front();  // drop oldest, like agent outbox
    outq_.push_back(std::move(frame));
  }

  void Wake() {
    char b = 1;
    ssize_t ignored = write(wake_w_, &b, 1);  // pipe full == already signalled
    (void)ignored;
  }
  void WakeLocked() { Wake(); }  // wake_w_ is const after Init

  // ---- I/O thread ----------------------------------------------------------

  void IoLoop() {
    auto backoff = kBackoffMin;
    while (!stop_.load()) {
      int fd = Connect();
      if (fd < 0) {
        WaitWake(backoff);
        backoff = std::min(backoff * 2, kBackoffMax);
        continue;
      }
      backoff = kBackoffMin;
      Session(fd);
      close(fd);
    }
  }

  int Connect() {
    sockaddr_un addr{};
    addr.sun_family = AF_UNIX;
    if (socket_path_.size() >= sizeof(addr.sun_path)) {
      if (!path_error_logged_) {
        path_error_logged_ = true;
        std::fprintf(stderr, "[birdman] BIRDMAN_SOCKET path too long (%zu bytes): %s\n",
                     socket_path_.size(), socket_path_.c_str());
      }
      return -1;
    }
    std::memcpy(addr.sun_path, socket_path_.c_str(), socket_path_.size() + 1);
    int fd = socket(AF_UNIX, SOCK_STREAM, 0);
    if (fd < 0) return -1;
#ifdef SO_NOSIGPIPE
    int one = 1;
    setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &one, sizeof(one));
#endif
    if (connect(fd, reinterpret_cast<sockaddr*>(&addr), sizeof(addr)) != 0) {
      Debug("connect %s: %s", socket_path_.c_str(), std::strerror(errno));
      close(fd);
      return -1;
    }
    fcntl(fd, F_SETFL, O_NONBLOCK);
    fcntl(fd, F_SETFD, FD_CLOEXEC);
    Debug("connected to agent socket %s", socket_path_.c_str());
    return fd;
  }

  // Waits for d or a wake byte (only used while disconnected).
  void WaitWake(std::chrono::milliseconds d) {
    pollfd pfd{wake_r_, POLLIN, 0};
    poll(&pfd, 1, static_cast<int>(d.count()));
    DrainWakePipe();
  }

  void DrainWakePipe() {
    char buf[64];
    while (read(wake_r_, buf, sizeof(buf)) > 0) {
    }
  }

  // One connection: hello + state replay, then reads/writes/timers until the
  // connection drops or Shutdown.
  void Session(int fd) {
    std::string pending;  // bytes to write, owned by the I/O thread
    std::string inbuf;
    {
      std::lock_guard<std::mutex> lock(mu_);
      connected_ = true;
      pending += json::Frame("hello", json::Obj().Str("sdk_version", SdkVersion()).Done());
      if (ready_) pending += json::Frame("ready", "{}");
      if (players_known_) pending += PlayersFrame(players_);
      next_players_at_ = Clock::now() + kPlayersEvery;
    }

    // stall_since answers "since when have we been unable to get these bytes
    // out", so it must start ticking when the buffer BECOMES non-empty -- not
    // when it was last seen empty. Those are the same instant only for a busy
    // link: an idle I/O thread does not loop, it sleeps inside poll(), so "last
    // seen empty" can be a minute old while the buffer filled a microsecond
    // ago. Getting that wrong killed the connection on the first frame after
    // any quiet spell longer than kWriteStall -- a pong, or the periodic
    // `players` -- which on a warm-pool server (quiet by definition) meant a
    // reconnect every keepalive, forever. had_pending is what makes the
    // distinction, and it is why the empty case clears it.
    auto stall_since = Clock::now();
    bool had_pending = false;
    bool alive = true;
    while (alive && !stop_.load()) {
      const auto now = Clock::now();
      bool wants_write;
      {
        std::lock_guard<std::mutex> lock(mu_);
        // Timer frames (metrics, periodic players) are regenerable state and
        // go straight to the session buffer. Ring frames (match_start/
        // match_end) stay in outq_ until the socket is actually writable, so
        // a connection that dies while we sleep does not eat them.
        FlushDueMetricsLocked(now, &pending);
        if (ready_ && now >= next_players_at_) {
          pending += PlayersFrame(players_);
          next_players_at_ = now + kPlayersEvery;
        }
        wants_write = !pending.empty() || !outq_.empty();
      }

      if (!wants_write) {
        stall_since = now;
        had_pending = false;
      } else if (!had_pending) {
        stall_since = now;  // the write starts here
        had_pending = true;
      }
      if (wants_write && now - stall_since > kWriteStall) {
        Debug("write stalled >3s, dropping connection");
        break;
      }

      pollfd pfds[2];
      pfds[0] = {fd, static_cast<short>(POLLIN | (wants_write ? POLLOUT : 0)), 0};
      pfds[1] = {wake_r_, POLLIN, 0};
      int rc = poll(pfds, 2, NextTimeoutMs(now, wants_write, stall_since));
      if (rc < 0 && errno != EINTR) break;

      if (pfds[1].revents & POLLIN) DrainWakePipe();

      if (pfds[0].revents & (POLLIN | POLLHUP | POLLERR)) {
        alive = ReadSome(fd, &inbuf, &pending);
      }
      if (alive && (pfds[0].revents & POLLOUT) != 0) {
        {
          std::lock_guard<std::mutex> lock(mu_);
          while (!outq_.empty()) {
            pending += outq_.front();
            outq_.pop_front();
          }
        }
        const size_t before = pending.size();
        if (!WriteSome(fd, &pending)) {
          alive = false;
        } else if (pending.size() != before) {
          stall_since = Clock::now();  // any progress resets the stall clock
        }
      }
    }

    if (stop_.load()) FlushOnShutdown(fd, &pending);
    {
      std::lock_guard<std::mutex> lock(mu_);
      connected_ = false;
    }
    Debug("agent socket disconnected");
  }

  // callers hold mu_
  void FlushDueMetricsLocked(Clock::time_point now, std::string* pending) {
    for (auto& kv : metrics_) {
      MetricSlot& slot = kv.second;
      if (slot.dirty && now - slot.last_send >= kMetricEvery) {
        slot.last_send = now;
        slot.dirty = false;
        *pending += MetricFrame(kv.first, slot.value);
      }
    }
  }

  int NextTimeoutMs(Clock::time_point now, bool has_pending, Clock::time_point stall_since) {
    auto deadline = Clock::time_point::max();
    {
      std::lock_guard<std::mutex> lock(mu_);
      if (ready_) deadline = std::min(deadline, next_players_at_);
      for (const auto& kv : metrics_) {
        if (kv.second.dirty) deadline = std::min(deadline, kv.second.last_send + kMetricEvery);
      }
    }
    if (has_pending) deadline = std::min(deadline, stall_since + kWriteStall);
    if (deadline == Clock::time_point::max()) return 60000;
    auto ms = std::chrono::duration_cast<std::chrono::milliseconds>(deadline - now).count();
    if (ms < 0) ms = 0;
    if (ms > 60000) ms = 60000;
    return static_cast<int>(ms);
  }

  bool ReadSome(int fd, std::string* inbuf, std::string* pending) {
    char buf[16 * 1024];
    for (;;) {
      ssize_t n = recv(fd, buf, sizeof(buf), 0);
      if (n > 0) {
        inbuf->append(buf, static_cast<size_t>(n));
        if (!ExtractLines(inbuf, pending)) return false;
        if (static_cast<size_t>(n) < sizeof(buf)) return true;
        continue;
      }
      if (n == 0) return false;  // EOF: agent closed / restarted
      if (errno == EAGAIN || errno == EWOULDBLOCK || errno == EINTR) return true;
      Debug("read: %s", std::strerror(errno));
      return false;
    }
  }

  bool ExtractLines(std::string* inbuf, std::string* pending) {
    size_t start = 0;
    for (;;) {
      size_t nl = inbuf->find('\n', start);
      if (nl == std::string::npos) break;
      HandleLine(std::string_view(inbuf->data() + start, nl - start), pending);
      start = nl + 1;
    }
    inbuf->erase(0, start);
    if (inbuf->size() > kMaxLine) {
      Debug("incoming line exceeds %zu bytes, dropping connection", kMaxLine);
      return false;
    }
    return true;
  }

  bool WriteSome(int fd, std::string* pending) {
    while (!pending->empty()) {
#ifdef MSG_NOSIGNAL
      ssize_t n = send(fd, pending->data(), pending->size(), MSG_NOSIGNAL);
#else
      ssize_t n = send(fd, pending->data(), pending->size(), 0);
#endif
      if (n > 0) {
        pending->erase(0, static_cast<size_t>(n));
        continue;
      }
      if (errno == EAGAIN || errno == EWOULDBLOCK || errno == EINTR) return true;
      Debug("write: %s", std::strerror(errno));
      return false;
    }
    return true;
  }

  // Bounded best-effort flush of buffered frames before Shutdown returns.
  void FlushOnShutdown(int fd, std::string* pending) {
    {
      std::lock_guard<std::mutex> lock(mu_);
      while (!outq_.empty()) {
        *pending += outq_.front();
        outq_.pop_front();
      }
    }
    const auto deadline = Clock::now() + kShutdownFlush;
    while (!pending->empty() && Clock::now() < deadline) {
      pollfd pfd{fd, POLLOUT, 0};
      auto left = std::chrono::duration_cast<std::chrono::milliseconds>(deadline - Clock::now());
      if (poll(&pfd, 1, std::max<int>(0, static_cast<int>(left.count()))) <= 0) break;
      if (!WriteSome(fd, pending)) break;
    }
  }

  // ---- incoming frames -------------------------------------------------------

  void HandleLine(std::string_view line, std::string* pending) {
    if (line.empty() || line.find_first_not_of(" \t\r") == std::string_view::npos) return;
    json::Value root;
    if (!json::Parse(line, &root) || !root.IsObject()) {
      Debug("bad frame: %.*s", static_cast<int>(std::min<size_t>(line.size(), 200)), line.data());
      return;
    }
    if (root.GetInt("v", -1) != 1) {
      Debug("envelope v=%lld ignored (sdk speaks v1)",
            static_cast<long long>(root.GetInt("v", -1)));
      return;
    }
    const std::string type = root.GetString("type");
    const json::Value* data = root.Get("data");
    static const json::Value kEmpty{};
    const json::Value& d = (data != nullptr && data->IsObject()) ? *data : kEmpty;

    if (type == "ping") {
      *pending += json::Frame("pong", "{}");
      return;
    }
    if (type == "allocated") {
      HandleAllocated(d);
      return;
    }
    if (type == "drain") {
      HandleDrain(d);
      return;
    }
    // forward-compat: unknown types are ignored (protocol.md §2)
    Debug("ignoring frame type %s", type.c_str());
  }

  void HandleAllocated(const json::Value& d) {
    AllocatedEvent ev;
    ev.match_id = d.GetString("match_id");
    ev.players_expected = static_cast<int>(d.GetInt("players_expected", 0));
    if (const json::Value* meta = d.Get("metadata"); meta != nullptr && meta->IsObject()) {
      for (const auto& kv : meta->object) {
        if (kv.second.type == json::Value::Type::kString) ev.metadata[kv.first] = kv.second.str;
      }
    }
    bool fresh = false;
    bool deliver = false;
    {
      std::lock_guard<std::mutex> lock(mu_);
      // The agent replays the last allocated on every reconnect: dedup by
      // match_id so the game sees each allocation exactly once.
      if (!(have_allocated_ && allocated_match_id_ == ev.match_id)) {
        have_allocated_ = true;
        allocated_match_id_ = ev.match_id;
        match_id_ = ev.match_id;
        fresh = true;
        if (cfg_.callback_mode == CallbackMode::kPoll) {
          PendingEvent pe;
          pe.allocated = ev;
          PushEventLocked(std::move(pe));
        } else {
          deliver = true;
        }
      }
    }
    Debug("allocated match_id=%s players_expected=%d%s", ev.match_id.c_str(),
          ev.players_expected, fresh ? "" : " (replay, ignored)");
    if (deliver && cfg_.on_allocated) cfg_.on_allocated(ev);  // no lock held
  }

  void HandleDrain(const json::Value& d) {
    DrainEvent ev;
    ev.deadline_seconds = d.GetNumber("deadline_s", 0.0);
    ev.reason = d.GetString("reason");
    bool fresh = false;
    bool deliver = false;
    {
      std::lock_guard<std::mutex> lock(mu_);
      // Same replay dedup as allocated: identical (deadline_s, reason) after
      // a reconnect is the agent replaying, not a new drain request.
      if (!(have_drain_ && drain_deadline_ == ev.deadline_seconds && drain_reason_ == ev.reason)) {
        have_drain_ = true;
        drain_deadline_ = ev.deadline_seconds;
        drain_reason_ = ev.reason;
        fresh = true;
        if (cfg_.callback_mode == CallbackMode::kPoll) {
          PendingEvent pe;
          pe.is_drain = true;
          pe.drain = ev;
          PushEventLocked(std::move(pe));
        } else {
          deliver = true;
        }
      }
    }
    Debug("drain deadline_s=%.0f reason=%s%s", ev.deadline_seconds, ev.reason.c_str(),
          fresh ? "" : " (replay, ignored)");
    if (deliver && cfg_.on_drain_requested) cfg_.on_drain_requested(ev);  // no lock held
  }

  // callers hold mu_
  void PushEventLocked(PendingEvent ev) {
    if (events_.size() >= kEventCap) events_.pop_front();
    events_.push_back(std::move(ev));
  }

  // ---- state -----------------------------------------------------------------

  Config cfg_;  // const after Init (callbacks invoked without the lock)
  std::string socket_path_;
  std::string server_id_;
  int port_ = 0;
  bool debug_ = false;
  bool path_error_logged_ = false;

  mutable std::mutex mu_;
  bool inited_ = false;
  bool shut_ = false;
  std::atomic<bool> managed_{false};
  std::atomic<bool> stop_{false};

  // game state (mu_)
  bool ready_ = false;
  bool players_known_ = false;
  int players_ = 0;
  std::string match_id_;
  bool have_allocated_ = false;
  std::string allocated_match_id_;
  bool have_drain_ = false;
  double drain_deadline_ = 0.0;
  std::string drain_reason_;

  // transport state (mu_)
  bool connected_ = false;
  std::deque<std::string> outq_;  // serialized frames, ring of kRingCap
  std::deque<PendingEvent> events_;
  std::map<std::string, MetricSlot> metrics_;
  Clock::time_point next_players_at_{};

  int wake_r_ = -1;
  int wake_w_ = -1;
  std::thread io_;
};

ServerLink::ServerLink() : impl_(new Impl()) {}
ServerLink::~ServerLink() = default;

bool ServerLink::Init() { return impl_->InitImpl(Config{}); }
bool ServerLink::Init(const Config& config) { return impl_->InitImpl(config); }
bool ServerLink::IsManaged() const { return impl_->IsManaged(); }
void ServerLink::NotifyReady() { impl_->NotifyReady(); }
void ServerLink::NotifyMatchStart() { impl_->NotifyMatchStart(); }
void ServerLink::NotifyMatchEnd(MatchResult result) { impl_->NotifyMatchEnd(result); }
void ServerLink::SetPlayerCount(int count) { impl_->SetPlayerCount(count); }
void ServerLink::ReportMetric(const std::string& name, double value) {
  impl_->ReportMetric(name, value);
}
int ServerLink::PollCallbacks() { return impl_->PollCallbacks(); }
void ServerLink::Shutdown() { impl_->ShutdownImpl(); }
std::string ServerLink::ServerId() const { return impl_->ServerIdImpl(); }
int ServerLink::Port() const { return impl_->PortImpl(); }
std::string ServerLink::MatchId() const { return impl_->MatchIdImpl(); }

}  // namespace birdman
