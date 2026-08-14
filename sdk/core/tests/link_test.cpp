// ServerLink behaviour: no-op mode, full lifecycle, keepalive, drain,
// poll-mode callbacks, reconnect with state replay, outgoing ring buffer,
// metric rate limiting, thread safety.
#include <unistd.h>

#include <atomic>
#include <cstdlib>
#include <string>
#include <thread>
#include <vector>

#include "birdman/birdman.h"
#include "framework.h"
#include "json.h"
#include "mock_agent.h"

using birdman::AllocatedEvent;
using birdman::Config;
using birdman::DrainEvent;
using birdman::MatchResult;
using birdman::ServerLink;

namespace {

void SleepMs(int ms) { usleep(ms * 1000); }

// Everything the SDK might legally emit, for Expect() calls that only care
// about one frame type.
const std::vector<std::string> kSkipAll = {"*"};

}  // namespace

// No env, no config -> no-op mode: every call is safe, zero sockets are
// touched, IsManaged() == false.
BT_TEST(noop_mode) {
  unsetenv("BIRDMAN_SOCKET");
  unsetenv("BIRDMAN_SERVER_ID");
  unsetenv("BIRDMAN_PORT");

  bt::MockAgent watchdog;  // nobody should ever knock here

  ServerLink link;
  // Calls before Init are no-ops too.
  link.NotifyReady();
  BT_CHECK(!link.IsManaged());

  BT_CHECK(!link.Init());
  BT_CHECK(!link.IsManaged());
  BT_CHECK_EQ(link.ServerId(), "");
  BT_CHECK_EQ(link.Port(), 0);
  BT_CHECK_EQ(link.MatchId(), "");

  link.NotifyReady();
  link.NotifyMatchStart();
  link.SetPlayerCount(3);
  link.ReportMetric("tick_ms", 16.6);
  link.NotifyMatchEnd(MatchResult::kCompleted);
  BT_CHECK_EQ(link.PollCallbacks(), 0);
  link.Shutdown();
  link.Shutdown();  // idempotent
  // Calls after Shutdown stay safe.
  link.NotifyReady();
  link.SetPlayerCount(1);

  // No connection attempt hit the (unrelated) socket during all of that.
  BT_CHECK(!watchdog.SawConnAttempt(200));
}

// Full managed cycle against the mock agent:
// hello -> ready -> allocated -> players -> match_start -> match_end.
BT_TEST(lifecycle) {
  bt::MockAgent mock;
  std::atomic<int> allocations{0};
  AllocatedEvent got_alloc;

  ServerLink link;
  Config cfg;
  cfg.socket_path = mock.Path();
  cfg.server_id = "srv-life";
  cfg.port = 7777;
  cfg.on_allocated = [&](const AllocatedEvent& ev) {
    got_alloc = ev;
    allocations.fetch_add(1);
    // Callbacks may call back into the link (documented, must not deadlock).
    link.NotifyMatchStart();
  };
  BT_CHECK(link.Init(cfg));
  BT_CHECK(link.IsManaged());
  BT_CHECK_EQ(link.ServerId(), "srv-life");
  BT_CHECK_EQ(link.Port(), 7777);

  mock.Accept();
  auto hello = mock.Expect("hello");
  BT_CHECK(!hello.GetString("sdk_version").empty());

  link.NotifyReady();
  mock.Expect("ready");

  mock.Send("allocated",
            "{\"match_id\":\"m-42\",\"players_expected\":2,\"metadata\":{\"mode\":\"dm\"}}");
  link.SetPlayerCount(1);
  BT_CHECK_EQ(mock.Expect("players").GetInt("count"), 1);
  // match_start was sent from inside the on_allocated callback.
  BT_CHECK_EQ(mock.Expect("match_start").GetString("match_id"), "m-42");
  BT_CHECK_EQ(allocations.load(), 1);
  BT_CHECK_EQ(got_alloc.match_id, "m-42");
  BT_CHECK_EQ(got_alloc.players_expected, 2);
  BT_CHECK_EQ(got_alloc.metadata.at("mode"), "dm");
  BT_CHECK_EQ(link.MatchId(), "m-42");

  link.SetPlayerCount(2);
  BT_CHECK_EQ(mock.Expect("players").GetInt("count"), 2);

  link.NotifyMatchEnd(MatchResult::kCompleted);
  auto end = mock.Expect("match_end");
  BT_CHECK_EQ(end.GetString("match_id"), "m-42");
  BT_CHECK_EQ(end.GetString("result"), "completed");

  link.Shutdown();
}

// Agent keepalive: every ping gets a pong.
BT_TEST(ping_pong) {
  bt::MockAgent mock;
  ServerLink link;
  Config cfg;
  cfg.socket_path = mock.Path();
  BT_CHECK(link.Init(cfg));
  mock.Accept();
  mock.Expect("hello");
  for (int i = 0; i < 3; ++i) {
    mock.Send("ping", "{}");
    mock.Expect("pong");
  }
  link.Shutdown();
}

// The keepalive has to survive QUIET, which is the state a warm pool server
// spends its whole life in. ping_pong above sends its pings back to back, so
// for a long time nothing here measured what happens when the link has had
// nothing to say for longer than the write-stall window: the stall clock was
// only refreshed by an iteration that saw an empty buffer, and an idle I/O
// thread does not iterate -- it sleeps inside poll(). The first frame produced
// after the quiet (this pong, or the periodic `players`) therefore looked like
// a write that had been stuck for the whole idle period, and the connection was
// dropped before the frame ever reached the socket. On the dev fleet that read
// as every dedik reconnecting to its agent every 10 seconds, forever.
//
// The sleep is the test: it must exceed kWriteStall (3s) to mean anything.
BT_TEST(ping_pong_after_idle) {
  bt::MockAgent mock;
  ServerLink link;
  Config cfg;
  cfg.socket_path = mock.Path();
  BT_CHECK(link.Init(cfg));
  mock.Accept();
  mock.Expect("hello");

  SleepMs(3500);  // > kWriteStall

  mock.Send("ping", "{}");
  mock.Expect("pong");
  // ...and on the SAME connection: a reconnect would answer the ping too (the
  // agent replays, liba re-hellos), so the pong alone is not proof of health.
  BT_CHECK(!mock.SawConnAttempt(200));

  link.Shutdown();
}

// Drain callback fires once per distinct request; the agent's replay of the
// same frame after reconnect is deduplicated.
BT_TEST(drain_callback) {
  bt::MockAgent mock;
  std::atomic<int> drains{0};
  DrainEvent last;

  ServerLink link;
  Config cfg;
  cfg.socket_path = mock.Path();
  cfg.on_drain_requested = [&](const DrainEvent& ev) {
    last = ev;
    drains.fetch_add(1);
  };
  BT_CHECK(link.Init(cfg));
  mock.Accept();
  mock.Expect("hello");

  mock.Send("drain", "{\"deadline_s\":30,\"reason\":\"deploy\"}");
  mock.Send("ping", "{}");
  mock.Expect("pong");
  BT_CHECK_EQ(drains.load(), 1);
  BT_CHECK_EQ(last.deadline_seconds, 30.0);
  BT_CHECK_EQ(last.reason, "deploy");

  // Identical frame again = replay -> ignored.
  mock.Send("drain", "{\"deadline_s\":30,\"reason\":\"deploy\"}");
  mock.Send("ping", "{}");
  mock.Expect("pong");
  BT_CHECK_EQ(drains.load(), 1);

  // A different drain is a new request.
  mock.Send("drain", "{\"deadline_s\":5,\"reason\":\"node drain\"}");
  mock.Send("ping", "{}");
  mock.Expect("pong");
  BT_CHECK_EQ(drains.load(), 2);
  BT_CHECK_EQ(last.deadline_seconds, 5.0);

  link.Shutdown();
}

// CallbackMode::kPoll: nothing fires until the game drains the queue on its
// own tick; order of events is preserved.
BT_TEST(poll_mode) {
  bt::MockAgent mock;
  std::vector<std::string> order;

  ServerLink link;
  Config cfg;
  cfg.socket_path = mock.Path();
  cfg.callback_mode = birdman::CallbackMode::kPoll;
  cfg.on_allocated = [&](const AllocatedEvent& ev) { order.push_back("allocated:" + ev.match_id); };
  cfg.on_drain_requested = [&](const DrainEvent& ev) { order.push_back("drain:" + ev.reason); };
  BT_CHECK(link.Init(cfg));
  mock.Accept();
  mock.Expect("hello");

  mock.Send("allocated", "{\"match_id\":\"m-p\",\"players_expected\":2}");
  mock.Send("drain", "{\"deadline_s\":60,\"reason\":\"deploy\"}");
  mock.Send("ping", "{}");
  mock.Expect("pong");  // both frames are processed by now

  BT_CHECK(order.empty());              // nothing fired on the I/O thread
  BT_CHECK_EQ(link.MatchId(), "m-p");   // state getters update regardless
  BT_CHECK_EQ(link.PollCallbacks(), 2); // game tick drains the queue
  BT_CHECK_EQ(order.size(), 2u);
  BT_CHECK_EQ(order[0], "allocated:m-p");
  BT_CHECK_EQ(order[1], "drain:deploy");
  BT_CHECK_EQ(link.PollCallbacks(), 0);

  link.Shutdown();
}

// Agent restart: the SDK reconnects with backoff and replays its state —
// hello, ready, players — and the agent's allocated replay is deduplicated.
BT_TEST(reconnect_replay) {
  bt::MockAgent mock;
  std::atomic<int> allocations{0};

  ServerLink link;
  Config cfg;
  cfg.socket_path = mock.Path();
  cfg.on_allocated = [&](const AllocatedEvent&) { allocations.fetch_add(1); };
  BT_CHECK(link.Init(cfg));
  mock.Accept();
  mock.Expect("hello");
  link.NotifyReady();
  mock.Expect("ready");
  link.SetPlayerCount(2);
  BT_CHECK_EQ(mock.Expect("players").GetInt("count"), 2);
  mock.Send("allocated", "{\"match_id\":\"m-r\",\"players_expected\":2}");
  mock.Send("ping", "{}");
  mock.Expect("pong");
  BT_CHECK_EQ(allocations.load(), 1);

  // Agent dies (listener gone), connection drops.
  mock.StopListening();
  mock.CloseConn();
  SleepMs(300);  // let the SDK notice EOF and start its backoff loop

  // Agent comes back: fresh socket at the same path.
  mock.Relisten();
  mock.Accept();

  // State replay, in order: hello -> ready -> players.
  BT_CHECK(!mock.Expect("hello").GetString("sdk_version").empty());
  mock.Expect("ready");
  BT_CHECK_EQ(mock.Expect("players").GetInt("count"), 2);

  // The real agent replays the last allocated on reconnect: must be deduped.
  mock.Send("allocated", "{\"match_id\":\"m-r\",\"players_expected\":2}");
  mock.Send("ping", "{}");
  mock.Expect("pong");
  BT_CHECK_EQ(allocations.load(), 1);
  BT_CHECK_EQ(link.MatchId(), "m-r");

  // Link still fully functional.
  link.SetPlayerCount(1);
  BT_CHECK_EQ(mock.Expect("players").GetInt("count"), 1);

  link.Shutdown();
}

// match_start/match_end emitted while the agent is down are ring-buffered
// and delivered on reconnect; ready is not re-advertised after match_end
// (one-shot server contract).
BT_TEST(ring_buffer) {
  bt::MockAgent mock;
  ServerLink link;
  Config cfg;
  cfg.socket_path = mock.Path();
  BT_CHECK(link.Init(cfg));
  mock.Accept();
  mock.Expect("hello");
  link.NotifyReady();
  mock.Expect("ready");
  mock.Send("allocated", "{\"match_id\":\"m-b\",\"players_expected\":1}");
  mock.Send("ping", "{}");
  mock.Expect("pong");

  mock.StopListening();
  mock.CloseConn();
  SleepMs(300);

  link.NotifyMatchStart();
  link.NotifyMatchEnd(MatchResult::kAborted);

  mock.Relisten();
  mock.Accept();
  mock.Expect("hello");
  // No ready replay after match_end; buffered frames arrive in order.
  BT_CHECK_EQ(mock.Expect("match_start").GetString("match_id"), "m-b");
  auto end = mock.Expect("match_end");
  BT_CHECK_EQ(end.GetString("match_id"), "m-b");
  BT_CHECK_EQ(end.GetString("result"), "aborted");

  link.Shutdown();
}

// The outgoing ring holds 256 frames; overflow drops the oldest.
BT_TEST(ring_overflow) {
  bt::MockAgent mock;
  ServerLink link;
  Config cfg;
  cfg.socket_path = mock.Path();
  BT_CHECK(link.Init(cfg));
  mock.Accept();
  mock.Expect("hello");

  mock.StopListening();
  mock.CloseConn();
  SleepMs(300);

  // 44 frames that must be dropped, then 256 that must survive.
  for (int i = 0; i < 44; ++i) link.NotifyMatchEnd(MatchResult::kCompleted);
  for (int i = 0; i < 256; ++i) link.NotifyMatchEnd(MatchResult::kAborted);

  mock.Relisten();
  mock.Accept();
  mock.Expect("hello");
  auto ends = mock.Collect("match_end", 500);
  BT_CHECK_EQ(ends.size(), 256u);
  for (const auto& e : ends) {
    BT_CHECK_EQ(e.GetString("result"), "aborted");
  }

  link.Shutdown();
}

// Metrics are rate-limited to 1/s per name with coalescing: the latest value
// always gets through, intermediate ones may be skipped.
BT_TEST(metric_rate_limit) {
  bt::MockAgent mock;
  ServerLink link;
  Config cfg;
  cfg.socket_path = mock.Path();
  BT_CHECK(link.Init(cfg));
  mock.Accept();
  mock.Expect("hello");

  link.ReportMetric("tick_ms", 1.0);
  link.ReportMetric("tick_ms", 2.0);  // within 1s -> coalesced
  link.ReportMetric("tick_ms", 3.0);  // latest wins
  link.ReportMetric("fps", 60.0);     // different name -> independent budget

  auto first = mock.Expect("metric", 5000, {"log"});
  BT_CHECK_EQ(first.GetString("name"), "tick_ms");
  BT_CHECK_EQ(first.GetNumber("value"), 1.0);
  auto second = mock.Expect("metric", 5000, {"log"});
  BT_CHECK_EQ(second.GetString("name"), "fps");
  BT_CHECK_EQ(second.GetNumber("value"), 60.0);
  // The coalesced tick_ms arrives once the 1s window passes — value 3, and
  // no value-2 frame before it.
  auto third = mock.Expect("metric", 5000, {"log"});
  BT_CHECK_EQ(third.GetString("name"), "tick_ms");
  BT_CHECK_EQ(third.GetNumber("value"), 3.0);

  link.Shutdown();
}

// Concurrent public calls + reconnect churn: correctness is "no crash, link
// still alive"; run under TSAN (BIRDMAN_SANITIZE=thread) to catch races.
BT_TEST(concurrency) {
  bt::MockAgent mock;
  ServerLink link;
  Config cfg;
  cfg.socket_path = mock.Path();
  cfg.on_allocated = [&](const AllocatedEvent&) { link.NotifyMatchStart(); };
  BT_CHECK(link.Init(cfg));
  mock.Accept();
  mock.Expect("hello");
  link.NotifyReady();
  mock.Expect("ready");

  std::atomic<bool> stop{false};
  std::vector<std::thread> workers;
  for (int t = 0; t < 4; ++t) {
    workers.emplace_back([&, t] {
      for (int i = 0; !stop.load() && i < 500; ++i) {
        link.SetPlayerCount(i % 8);
        link.ReportMetric("m" + std::to_string(t), static_cast<double>(i));
        if (i % 100 == 0) link.ReportMetric("tick_ms", 16.0 + t);
        std::this_thread::yield();
      }
    });
  }

  // Reconnect churn while the workers hammer the API.
  mock.Send("allocated", "{\"match_id\":\"m-c\",\"players_expected\":8}");
  for (int i = 0; i < 3; ++i) {
    SleepMs(50);
    mock.CloseConn();
    mock.Accept();  // SDK lands in the backlog and reconnects immediately
  }

  stop.store(true);
  for (auto& w : workers) w.join();

  // Link is still alive and ordered after the churn.
  mock.Send("ping", "{}");
  mock.Expect("pong", 5000, kSkipAll);
  link.SetPlayerCount(42);
  bool saw42 = false;
  for (int i = 0; i < 10 && !saw42; ++i) {
    auto players = mock.Expect("players", 5000, kSkipAll);
    saw42 = players.GetInt("count") == 42;
  }
  BT_CHECK(saw42);

  link.Shutdown();
}
