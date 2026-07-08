// Internal JSON writer/parser tests.
#include <string>

#include "framework.h"
#include "json.h"

using birdman::json::Obj;
using birdman::json::Parse;
using birdman::json::Value;

BT_TEST(json_writer) {
  BT_CHECK_EQ(Obj().Done(), "{}");
  BT_CHECK_EQ(Obj().Str("k", "v").Int("n", 42).Done(), "{\"k\":\"v\",\"n\":42}");
  // Escaping: quotes, backslashes, control characters.
  BT_CHECK_EQ(Obj().Str("s", "a\"b\\c\nd\te").Done(), "{\"s\":\"a\\\"b\\\\c\\nd\\te\"}");
  BT_CHECK_EQ(Obj().Str("s", std::string("x\x01y", 3)).Done(), "{\"s\":\"x\\u0001y\"}");
  // Numbers: whole doubles print as integers; NaN/Inf clamp to 0 (valid JSON).
  BT_CHECK_EQ(Obj().Num("v", 16.5).Done(), "{\"v\":16.5}");
  BT_CHECK_EQ(Obj().Num("v", 2.0).Done(), "{\"v\":2}");
  BT_CHECK_EQ(Obj().Num("v", -0.25).Done(), "{\"v\":-0.25}");
  BT_CHECK_EQ(Obj().Num("v", 0.0 / 0.0).Done(), "{\"v\":0}");
  BT_CHECK_EQ(Obj().Int("v", -7).Done(), "{\"v\":-7}");
  // Raw splicing for nested values.
  BT_CHECK_EQ(Obj().Raw("meta", "{\"a\":\"b\"}").Done(), "{\"meta\":{\"a\":\"b\"}}");
  // Frame envelope.
  BT_CHECK_EQ(birdman::json::Frame("ready", "{}"), "{\"v\":1,\"type\":\"ready\",\"data\":{}}\n");

  // Round-trip: writer output parses back to the same values.
  Value v;
  BT_CHECK(Parse(Obj().Str("name", "tick_ms").Num("value", 16.6).Done(), &v));
  BT_CHECK_EQ(v.GetString("name"), "tick_ms");
  BT_CHECK(v.GetNumber("value") > 16.59 && v.GetNumber("value") < 16.61);
}

BT_TEST(json_parser) {
  Value v;
  // The canonical envelope shape.
  BT_CHECK(Parse("{\"v\":1,\"type\":\"allocated\",\"data\":{\"match_id\":\"m-1\","
                 "\"players_expected\":2,\"metadata\":{\"mode\":\"dm\"}}}",
                 &v));
  BT_CHECK_EQ(v.GetInt("v"), 1);
  BT_CHECK_EQ(v.GetString("type"), "allocated");
  const Value* data = v.Get("data");
  BT_CHECK(data != nullptr && data->IsObject());
  BT_CHECK_EQ(data->GetString("match_id"), "m-1");
  BT_CHECK_EQ(data->GetInt("players_expected"), 2);
  const Value* meta = data->Get("metadata");
  BT_CHECK(meta != nullptr && meta->IsObject());
  BT_CHECK_EQ(meta->GetString("mode"), "dm");

  // Scalars, arrays, escapes, unicode.
  BT_CHECK(Parse(" { \"a\" : [1, -2.5, true, false, null, \"x\"] } ", &v));
  const Value* arr = v.Get("a");
  BT_CHECK(arr != nullptr && arr->type == Value::Type::kArray);
  BT_CHECK_EQ(static_cast<int>(arr->array.size()), 6);
  BT_CHECK_EQ(arr->array[1].number, -2.5);
  BT_CHECK(arr->array[2].boolean);
  BT_CHECK(Parse("{\"s\":\"a\\n\\t\\\"\\\\\\u0041\\u00e9\\ud83d\\ude00\"}", &v));
  BT_CHECK_EQ(v.GetString("s"), std::string("a\n\t\"\\A\xc3\xa9\xf0\x9f\x98\x80"));

  // Absent/mistyped members fall back to defaults.
  BT_CHECK(Parse("{\"n\":\"not a number\"}", &v));
  BT_CHECK_EQ(v.GetInt("n", -1), -1);
  BT_CHECK_EQ(v.GetString("missing", "def"), "def");
  BT_CHECK(v.Get("missing") == nullptr);

  // Numbers in exponent form (Go's encoding/json may emit them).
  BT_CHECK(Parse("{\"v\":1.5e3}", &v));
  BT_CHECK_EQ(v.GetNumber("v"), 1500.0);

  // Malformed input is rejected, not crashed on.
  const char* bad[] = {
      "",  "{", "}", "{\"a\":}", "{\"a\":1,}", "{'a':1}",  "{\"a\":01e}",
      "{\"a\":\"unterminated}", "{\"a\":\"bad\\q\"}", "{\"a\":\"\\ud800\"}", "nulll",
      "{\"a\":1} trailing",
  };
  for (const char* s : bad) {
    BT_CHECK(!Parse(s, &v));
  }

  // Deep nesting is capped, not stack-overflowed.
  std::string deep;
  for (int i = 0; i < 200; ++i) deep += "[";
  BT_CHECK(!Parse(deep, &v));

  // Duplicate keys: last one wins (matches encoding/json).
  BT_CHECK(Parse("{\"a\":1,\"a\":2}", &v));
  BT_CHECK_EQ(v.GetInt("a"), 2);
}
