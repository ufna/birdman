// In-process mock of the birdman agent's per-server unix socket for tests:
// listens, accepts (re)connections, asserts on incoming NDJSON frames and
// injects agent->liba frames. Mirrors agent/internal/uds/server.go behaviour
// closely enough for SDK-side testing; the Go CLI twin for manual runs lives
// in sdk/mockagent.
#ifndef BIRDMAN_TESTS_MOCK_AGENT_H_
#define BIRDMAN_TESTS_MOCK_AGENT_H_

#include <fcntl.h>
#include <poll.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/un.h>
#include <unistd.h>

#include <cerrno>
#include <chrono>
#include <cstdlib>
#include <cstring>
#include <string>
#include <vector>

#include "framework.h"
#include "json.h"

namespace bt {

// Temp dir short enough for sockaddr_un (sun_path is 104 bytes on macOS).
inline std::string MakeSockDir() {
  const char* env = std::getenv("TMPDIR");
  std::string base = (env != nullptr && env[0] != '\0') ? env : "/tmp";
  if (base.back() != '/') base += '/';
  if (base.size() > 60) base = "/tmp/";
  std::string tmpl = base + "birdsdk.XXXXXX";
  std::vector<char> buf(tmpl.begin(), tmpl.end());
  buf.push_back('\0');
  if (mkdtemp(buf.data()) == nullptr) BT_FAIL("mkdtemp failed");
  return std::string(buf.data());
}

class MockAgent {
 public:
  MockAgent() {
    dir_ = MakeSockDir();
    path_ = dir_ + "/agent.sock";
    ln_ = ::socket(AF_UNIX, SOCK_STREAM, 0);
    BT_CHECK(ln_ >= 0);
    sockaddr_un addr{};
    addr.sun_family = AF_UNIX;
    BT_CHECK(path_.size() < sizeof(addr.sun_path));
    std::memcpy(addr.sun_path, path_.c_str(), path_.size() + 1);
    BT_CHECK(::bind(ln_, reinterpret_cast<sockaddr*>(&addr), sizeof(addr)) == 0);
    BT_CHECK(::listen(ln_, 8) == 0);
  }

  ~MockAgent() {
    CloseConn();
    if (ln_ >= 0) ::close(ln_);
    ::unlink(path_.c_str());
    ::rmdir(dir_.c_str());
  }

  MockAgent(const MockAgent&) = delete;
  MockAgent& operator=(const MockAgent&) = delete;

  const std::string& Path() const { return path_; }

  // Waits for the next liba connection; replaces the previous one (like the
  // real agent's accept loop).
  void Accept(int timeout_ms = 5000) {
    pollfd pfd{ln_, POLLIN, 0};
    if (::poll(&pfd, 1, timeout_ms) <= 0) BT_FAIL("mock agent: accept timeout");
    int conn = ::accept(ln_, nullptr, nullptr);
    BT_CHECK(conn >= 0);
    CloseConn();
    conn_ = conn;
    rbuf_.clear();
  }

  void CloseConn() {
    if (conn_ >= 0) {
      ::close(conn_);
      conn_ = -1;
    }
  }

  // Simulates the agent process dying: no listener, connect() fails. Call
  // before CloseConn() so the SDK cannot sneak into the listen backlog.
  void StopListening() {
    if (ln_ >= 0) {
      ::close(ln_);
      ln_ = -1;
    }
    ::unlink(path_.c_str());
  }

  // Simulates the restarted agent recreating the socket (fresh inode at the
  // same path — the per-server dir bind-mount design, protocol.md §2).
  void Relisten() {
    BT_CHECK(ln_ < 0);
    ln_ = ::socket(AF_UNIX, SOCK_STREAM, 0);
    BT_CHECK(ln_ >= 0);
    sockaddr_un addr{};
    addr.sun_family = AF_UNIX;
    std::memcpy(addr.sun_path, path_.c_str(), path_.size() + 1);
    BT_CHECK(::bind(ln_, reinterpret_cast<sockaddr*>(&addr), sizeof(addr)) == 0);
    BT_CHECK(::listen(ln_, 8) == 0);
  }

  // True if a connection attempt lands within timeout_ms (no accept).
  bool SawConnAttempt(int timeout_ms) {
    pollfd pfd{ln_, POLLIN, 0};
    return ::poll(&pfd, 1, timeout_ms) > 0;
  }

  // Reads frames until one of type `type` arrives; frame types listed in
  // `skip` are ignored on the way (metric/log are periodic noise; "*" skips
  // everything else). Anything else fails the test. Returns the frame's data
  // (empty object if none).
  birdman::json::Value Expect(const std::string& type, int timeout_ms = 5000,
                              const std::vector<std::string>& skip = {"metric", "log"}) {
    const auto deadline =
        std::chrono::steady_clock::now() + std::chrono::milliseconds(timeout_ms);
    for (;;) {
      std::string line;
      if (!NextLine(&line, deadline)) {
        BT_FAIL("mock agent: timeout/close waiting for frame \"" + type + "\"");
      }
      birdman::json::Value root;
      if (!birdman::json::Parse(line, &root) || !root.IsObject()) {
        BT_FAIL("mock agent: bad frame: " + line);
      }
      if (root.GetInt("v", -1) != 1) BT_FAIL("mock agent: frame without v=1: " + line);
      const std::string got = root.GetString("type");
      bool skipped = false;
      for (const auto& s : skip) {
        if (got == s || (s == "*" && got != type)) {
          skipped = true;
          break;
        }
      }
      if (skipped) continue;
      if (got != type) {
        BT_FAIL("mock agent: expected \"" + type + "\", got \"" + got + "\" (" + line + ")");
      }
      const birdman::json::Value* data = root.Get("data");
      if (data != nullptr && data->IsObject()) return *data;
      birdman::json::Value empty;
      empty.type = birdman::json::Value::Type::kObject;
      return empty;
    }
  }

  // Collects frames of type `type` until the connection goes quiet for
  // `quiet_ms`; used for ring-buffer flush assertions.
  std::vector<birdman::json::Value> Collect(const std::string& type, int quiet_ms = 500) {
    std::vector<birdman::json::Value> out;
    for (;;) {
      const auto deadline =
          std::chrono::steady_clock::now() + std::chrono::milliseconds(quiet_ms);
      std::string line;
      if (!NextLine(&line, deadline)) return out;
      birdman::json::Value root;
      if (!birdman::json::Parse(line, &root) || !root.IsObject()) continue;
      if (root.GetString("type") != type) continue;
      const birdman::json::Value* data = root.Get("data");
      birdman::json::Value v;
      v.type = birdman::json::Value::Type::kObject;
      if (data != nullptr) v = *data;
      out.push_back(std::move(v));
    }
  }

  void Send(const std::string& type, const std::string& data_json) {
    SendRaw(birdman::json::Frame(type, data_json));
  }

  // Writes one raw line (no framing added; caller includes the trailing \n).
  void SendRaw(const std::string& line) {
    BT_CHECK(conn_ >= 0);
    size_t off = 0;
    while (off < line.size()) {
#ifdef MSG_NOSIGNAL
      ssize_t n = ::send(conn_, line.data() + off, line.size() - off, MSG_NOSIGNAL);
#else
      ssize_t n = ::send(conn_, line.data() + off, line.size() - off, 0);
#endif
      if (n < 0 && (errno == EAGAIN || errno == EINTR)) continue;
      if (n <= 0) BT_FAIL("mock agent: send failed");
      off += static_cast<size_t>(n);
    }
  }

 private:
  // Reads until a full line is available or deadline passes.
  bool NextLine(std::string* out, std::chrono::steady_clock::time_point deadline) {
    for (;;) {
      size_t nl = rbuf_.find('\n');
      if (nl != std::string::npos) {
        out->assign(rbuf_, 0, nl);
        rbuf_.erase(0, nl + 1);
        return true;
      }
      if (conn_ < 0) return false;
      auto left = std::chrono::duration_cast<std::chrono::milliseconds>(
                      deadline - std::chrono::steady_clock::now())
                      .count();
      if (left <= 0) return false;
      pollfd pfd{conn_, POLLIN, 0};
      int rc = ::poll(&pfd, 1, static_cast<int>(left));
      if (rc <= 0) return false;
      char buf[8192];
      ssize_t n = ::recv(conn_, buf, sizeof(buf), 0);
      if (n <= 0) return false;  // closed
      rbuf_.append(buf, static_cast<size_t>(n));
    }
  }

  std::string dir_;
  std::string path_;
  int ln_ = -1;
  int conn_ = -1;
  std::string rbuf_;
};

}  // namespace bt

#endif  // BIRDMAN_TESTS_MOCK_AGENT_H_
