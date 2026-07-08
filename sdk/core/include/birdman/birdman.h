// birdman/birdman.h — server-side SDK for birdman-managed dedicated servers.
//
// C++17, POSIX + standard library only, no external dependencies. Implements
// the liba side of the liba<->agent contract (docs/specs/protocol.md §2):
// NDJSON envelopes {"v":1,"type":...,"data":{...}} over a unix socket the
// agent listens on.
//
// CONTRACT FREEZE: this header is the SDK v0 contract (frozen 2026-07-08,
// docs/specs/sdk.md §2/§5). Changes are additive only: new methods, new
// Config fields with safe defaults, new event fields. Existing signatures
// and semantics do not change until v2.
//
// Quick start (see sdk/example/main.cpp for a complete game):
//
//   birdman::ServerLink link;
//   birdman::Config cfg;
//   cfg.on_allocated = [&](const birdman::AllocatedEvent& ev) { ... };
//   cfg.on_drain_requested = [&](const birdman::DrainEvent& ev) { ... };
//   link.Init(cfg);                  // env BIRDMAN_* absent -> harmless no-op
//   ...
//   link.NotifyReady();              // map loaded, ports listening (<=30s from start)
//   ...                              // on_allocated fires -> players connect
//   link.NotifyMatchStart();
//   link.SetPlayerCount(2);
//   ...
//   link.NotifyMatchEnd(birdman::MatchResult::kCompleted);
//   link.Shutdown();                 // then exit(0): dedicated servers are one-shot
//
#ifndef BIRDMAN_BIRDMAN_H_
#define BIRDMAN_BIRDMAN_H_

#include <functional>
#include <map>
#include <memory>
#include <string>

// SDK semver; the wire value is "birdman-cpp/<version>" (hello.sdk_version).
#define BIRDMAN_SDK_VERSION_MAJOR 0
#define BIRDMAN_SDK_VERSION_MINOR 1
#define BIRDMAN_SDK_VERSION_PATCH 0
#define BIRDMAN_SDK_VERSION "0.1.0"

namespace birdman {

// SdkVersion returns the full wire identifier, e.g. "birdman-cpp/0.1.0".
std::string SdkVersion();

// Match outcome reported in match_end.result.
enum class MatchResult {
  kCompleted,  // wire: "completed"
  kAborted,    // wire: "aborted"
};

// AllocatedEvent: the master allocated a match to this server
// (agent frame `allocated`). Delivered exactly once per distinct match_id:
// the agent replays the last allocated frame on every reconnect, the SDK
// deduplicates replays by match_id.
struct AllocatedEvent {
  std::string match_id;
  int players_expected = 0;  // 0 = unknown (external matchmaker via REST)
  std::map<std::string, std::string> metadata;  // may be empty; v0 agent sends none
};

// DrainEvent: finish the current match within deadline_seconds and exit;
// do not start new rounds (agent frame `drain`). Replays after reconnect
// are deduplicated; note the deadline is not rebased — it counts from the
// original delivery.
struct DrainEvent {
  double deadline_seconds = 0.0;  // 0 = exit as soon as reasonably possible
  std::string reason;             // e.g. "deploy", "node drain"; may be empty
};

// How allocated/drain events reach the game.
enum class CallbackMode {
  // Callbacks fire on the SDK I/O thread the moment a frame arrives.
  // Lowest latency; your handlers MUST be thread-safe (or immediately
  // marshal to the game thread — the UE plugin does exactly that).
  kDispatch,
  // Events are queued inside the SDK; nothing is called until the game
  // drains the queue with PollCallbacks() on its own tick. Game-thread
  // friendly: handlers run on whichever thread calls PollCallbacks().
  kPoll,
};

// Init-time configuration. Everything has a safe default; new fields are
// added only with defaults that preserve v0 behaviour.
struct Config {
  CallbackMode callback_mode = CallbackMode::kDispatch;

  // Event handlers (see CallbackMode for threading). May be empty: state
  // getters (MatchId()...) are updated either way, missed events are not
  // an error. Handlers may call ServerLink methods (no deadlock).
  std::function<void(const AllocatedEvent&)> on_allocated;
  std::function<void(const DrainEvent&)> on_drain_requested;

  // Overrides for tests/embedding. Empty/zero -> taken from the environment:
  // BIRDMAN_SOCKET, BIRDMAN_SERVER_ID, BIRDMAN_PORT. Managed mode is keyed
  // on the socket path alone: no BIRDMAN_SOCKET (and no override) -> no-op.
  std::string socket_path;
  std::string server_id;
  int port = 0;
};

// ServerLink is the game's connection to the local birdman agent.
// One instance per process. All public methods are thread-safe.
//
// No-op mode (mandatory property): when BIRDMAN_SOCKET is absent —
// local runs, PIE, CI — Init() configures nothing, IsManaged() == false,
// and every method is a safe no-op that performs zero network operations.
// The same game binary runs managed and standalone without ifdefs.
//
// Transport (all internal, the game never sees it): connects to the agent
// socket with exponential backoff and reconnects forever; sends
// hello{sdk_version} on every (re)connect, then replays current state
// (ready, player count); buffers match_start/match_end frames in a
// 256-entry ring while disconnected; answers agent ping with pong;
// resends players at least every 10s; rate-limits metrics to 1/s per name
// (latest value wins); ignores unknown incoming frame types.
class ServerLink {
 public:
  ServerLink();
  ~ServerLink();  // implies Shutdown()

  ServerLink(const ServerLink&) = delete;
  ServerLink& operator=(const ServerLink&) = delete;

  // Init configures the link and, in managed mode, starts the I/O thread.
  // Returns IsManaged(): true = running under a birdman agent. Calling
  // Init again is a no-op returning the current mode. Not restartable
  // after Shutdown().
  bool Init();                     // autoconfig from env, default Config
  bool Init(const Config& config);

  // True when running under a birdman agent (socket configured and Init
  // was called). Constant for the lifetime of the process.
  bool IsManaged() const;

  // Lifecycle notifications (game -> agent). See docs/specs/sdk.md §2 for
  // the obligations; short form:
  //  - NotifyReady: server can accept a match; required <=30s from process
  //    start or the agent declares the server failed.
  //  - NotifyMatchStart: the match actually began (players connected).
  //    Call after on_allocated so match_id is known.
  //  - NotifyMatchEnd: match over; after this the process must exit by
  //    itself — dedicated servers are one-shot, the fleet recreates the
  //    slot. The link stops advertising ready after match_end.
  void NotifyReady();
  void NotifyMatchStart();
  void NotifyMatchEnd(MatchResult result);

  // Current player count; call on every change (join/leave). The SDK also
  // resends the last value periodically as the protocol keepalive.
  void SetPlayerCount(int count);

  // Custom gauge, e.g. tick_ms. Sent at most once per second per name;
  // intermediate values are coalesced (latest wins), nothing is lost on
  // rate limiting except staleness.
  void ReportMetric(const std::string& name, double value);

  // CallbackMode::kPoll only: invokes queued on_allocated/on_drain_requested
  // handlers on the calling thread, in arrival order. Returns the number of
  // events dispatched. In kDispatch mode (and no-op mode) returns 0. Call
  // once per game tick.
  int PollCallbacks();

  // Stops the I/O thread, flushes pending frames (bounded, <=1s), closes
  // the socket. Idempotent. Safe to skip before a hard exit — the agent
  // treats a clean process exit as authoritative anyway.
  void Shutdown();

  // Introspection. ServerId/Port come from BIRDMAN_SERVER_ID/BIRDMAN_PORT
  // (or Config overrides): the id the fleet knows this server by and the
  // UDP/TCP port the game must bind. MatchId is the last allocated match
  // ("" until on_allocated).
  std::string ServerId() const;
  int Port() const;
  std::string MatchId() const;

 private:
  class Impl;
  std::unique_ptr<Impl> impl_;
};

}  // namespace birdman

#endif  // BIRDMAN_BIRDMAN_H_
