// Wire compatibility against the golden frames of docs/specs/protocol.md §2.
// Outgoing frames the SDK serializes must match the canonical frames the real
// agent parses (agent/internal/uds/server.go); incoming golden frames must
// drive the SDK exactly as specified. Comparison is on parsed JSON (field
// names/types), not bytes.
#include <atomic>
#include <string>
#include <vector>

#include "birdman/birdman.h"
#include "framework.h"
#include "json.h"
#include "mock_agent.h"

using birdman::json::Parse;
using birdman::json::Value;

namespace {

bool JsonEq(const Value& a, const Value& b) {
  if (a.type != b.type) return false;
  switch (a.type) {
    case Value::Type::kNull:
      return true;
    case Value::Type::kBool:
      return a.boolean == b.boolean;
    case Value::Type::kNumber:
      return a.number == b.number;
    case Value::Type::kString:
      return a.str == b.str;
    case Value::Type::kArray: {
      if (a.array.size() != b.array.size()) return false;
      for (size_t i = 0; i < a.array.size(); ++i) {
        if (!JsonEq(a.array[i], b.array[i])) return false;
      }
      return true;
    }
    case Value::Type::kObject: {
      // Order-insensitive; both sides come from the same parser (last
      // duplicate wins), so key-by-key lookup is sound.
      if (a.object.size() != b.object.size()) return false;
      for (const auto& kv : a.object) {
        const Value* other = b.Get(kv.first);
        if (other == nullptr || !JsonEq(kv.second, *other)) return false;
      }
      return true;
    }
  }
  return false;
}

void ExpectWire(bt::MockAgent& mock, const std::string& type, const std::string& golden) {
  Value got = mock.Expect(type);
  Value want;
  BT_CHECK(Parse(golden, &want));
  if (!JsonEq(got, want)) {
    BT_FAIL("wire mismatch for \"" + type + "\": golden data " + golden);
  }
}

}  // namespace

// liba -> agent: every frame type the SDK emits, against protocol.md §2.
BT_TEST(golden_outgoing) {
  bt::MockAgent mock;
  birdman::ServerLink link;
  birdman::Config cfg;
  cfg.socket_path = mock.Path();
  cfg.server_id = "srv-1";
  cfg.port = 7777;
  BT_CHECK(link.Init(cfg));
  BT_CHECK(link.IsManaged());
  mock.Accept();

  // hello {sdk_version} — right after connect.
  ExpectWire(mock, "hello",
             std::string("{\"sdk_version\":\"") + birdman::SdkVersion() + "\"}");
  BT_CHECK_EQ(birdman::SdkVersion(), "birdman-cpp/" BIRDMAN_SDK_VERSION);

  // ready {}
  link.NotifyReady();
  ExpectWire(mock, "ready", "{}");

  // players {count}
  link.SetPlayerCount(2);
  ExpectWire(mock, "players", "{\"count\":2}");

  // allocated drives match_id for match_start/match_end.
  mock.Send("allocated", "{\"match_id\":\"m-1\",\"players_expected\":2}");

  // match_start {match_id}
  // (wait until allocated is processed so match_id is filled in)
  for (int i = 0; i < 200 && link.MatchId().empty(); ++i) {
    usleep(10 * 1000);
  }
  BT_CHECK_EQ(link.MatchId(), "m-1");
  link.NotifyMatchStart();
  ExpectWire(mock, "match_start", "{\"match_id\":\"m-1\"}");

  // metric {name, value}
  link.ReportMetric("tick_ms", 16.5);
  Value metric = mock.Expect("metric", 5000, {"players", "log"});
  Value want_metric;
  BT_CHECK(Parse("{\"name\":\"tick_ms\",\"value\":16.5}", &want_metric));
  BT_CHECK(JsonEq(metric, want_metric));

  // pong {} — answer to agent ping.
  mock.Send("ping", "{}");
  ExpectWire(mock, "pong", "{}");

  // match_end {match_id, result}
  link.NotifyMatchEnd(birdman::MatchResult::kCompleted);
  ExpectWire(mock, "match_end", "{\"match_id\":\"m-1\",\"result\":\"completed\"}");

  link.Shutdown();
}

// agent -> liba: golden incoming frames byte-for-byte as the agent writes
// them; plus forward-compat rules (unknown type, wrong envelope version,
// malformed line, replay dedup).
BT_TEST(golden_incoming) {
  std::atomic<int> allocations{0};
  std::atomic<int> drains{0};
  birdman::AllocatedEvent last_alloc;
  birdman::DrainEvent last_drain;

  bt::MockAgent mock;
  birdman::ServerLink link;
  birdman::Config cfg;
  cfg.socket_path = mock.Path();
  cfg.on_allocated = [&](const birdman::AllocatedEvent& ev) {
    last_alloc = ev;
    allocations.fetch_add(1);
  };
  cfg.on_drain_requested = [&](const birdman::DrainEvent& ev) {
    last_drain = ev;
    drains.fetch_add(1);
  };
  BT_CHECK(link.Init(cfg));
  mock.Accept();
  mock.Expect("hello");

  // Exactly what the v0 agent sends (uds.Server.SendAllocated — no metadata).
  mock.SendRaw("{\"v\":1,\"type\":\"allocated\",\"data\":{\"match_id\":\"m-7\",\"players_expected\":2}}\n");
  // Noise the SDK must survive: unknown type, foreign envelope version,
  // malformed JSON, blank line (protocol.md §2 forward-compat rules).
  mock.SendRaw("{\"v\":1,\"type\":\"verify_token\",\"data\":{\"player_id\":\"p1\",\"token\":\"t\"}}\n");
  mock.SendRaw("{\"v\":2,\"type\":\"allocated\",\"data\":{\"match_id\":\"from-the-future\"}}\n");
  mock.SendRaw("not json at all\n");
  mock.SendRaw("\n");
  // Replay of the same allocated (agent replays on reconnect) — deduped.
  mock.SendRaw("{\"v\":1,\"type\":\"allocated\",\"data\":{\"match_id\":\"m-7\",\"players_expected\":2}}\n");
  // drain with metadata-rich payload per protocol table.
  mock.SendRaw("{\"v\":1,\"type\":\"drain\",\"data\":{\"deadline_s\":120,\"reason\":\"deploy\"}}\n");
  // ping -> pong proves the whole batch above was processed in order.
  mock.Send("ping", "{}");
  mock.Expect("pong");

  BT_CHECK_EQ(allocations.load(), 1);
  BT_CHECK_EQ(last_alloc.match_id, "m-7");
  BT_CHECK_EQ(last_alloc.players_expected, 2);
  BT_CHECK(last_alloc.metadata.empty());  // v0 agent sends no metadata
  BT_CHECK_EQ(drains.load(), 1);
  BT_CHECK_EQ(last_drain.deadline_seconds, 120.0);
  BT_CHECK_EQ(last_drain.reason, "deploy");
  BT_CHECK_EQ(link.MatchId(), "m-7");

  // allocated with metadata (protocol table full shape) — a new match id is
  // a fresh allocation, metadata comes through as strings.
  mock.SendRaw("{\"v\":1,\"type\":\"allocated\",\"data\":{\"match_id\":\"m-8\","
               "\"players_expected\":0,\"metadata\":{\"mode\":\"dm\",\"map\":\"arena\"}}}\n");
  mock.Send("ping", "{}");
  mock.Expect("pong");
  BT_CHECK_EQ(allocations.load(), 2);
  BT_CHECK_EQ(last_alloc.match_id, "m-8");
  BT_CHECK_EQ(last_alloc.players_expected, 0);
  BT_CHECK_EQ(last_alloc.metadata.at("mode"), "dm");
  BT_CHECK_EQ(last_alloc.metadata.at("map"), "arena");

  link.Shutdown();
}
