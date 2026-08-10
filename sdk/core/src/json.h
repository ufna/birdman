// Internal tiny JSON reader/writer for the flat liba<->agent envelope
// (docs/specs/protocol.md §2). Not a general-purpose JSON library: enough to
// emit our outgoing frames and to parse any well-formed incoming frame
// (nested objects/arrays are parsed for forward-compat and skipped by the
// caller). No external dependencies.
#ifndef BIRDMAN_SRC_JSON_H_
#define BIRDMAN_SRC_JSON_H_

#include <cstdint>
#include <string>
#include <string_view>
#include <utility>
#include <vector>

namespace birdman {
namespace json {

// ---- writer ----------------------------------------------------------------

// Appends s as a quoted JSON string (escapes ", \, control chars).
void AppendString(std::string* out, std::string_view s);

// Appends v as a JSON number. NaN/Inf (not representable in JSON) become 0.
// Uses the shortest round-trip form where the toolchain provides it.
void AppendNumber(std::string* out, double v);
void AppendNumber(std::string* out, int64_t v);

// Builder for one flat JSON object: Obj().Str("k","v").Int("n",1).Done()
// -> {"k":"v","n":1}. Raw() splices a pre-serialized value (nested object).
class Obj {
 public:
  Obj() : buf_("{") {}
  Obj& Str(std::string_view key, std::string_view value);
  Obj& Int(std::string_view key, int64_t value);
  Obj& Num(std::string_view key, double value);
  Obj& Raw(std::string_view key, std::string_view json_value);
  std::string Done();  // consumes the builder (one-shot)

 private:
  void Key(std::string_view key);
  std::string buf_;
  bool first_ = true;
};

// One wire frame: {"v":1,"type":<type>,"data":<data_json>} + '\n'.
// data_json must be a serialized JSON value; pass "{}" when there is no data.
std::string Frame(std::string_view type, std::string_view data_json);

// ---- reader ----------------------------------------------------------------

// Parsed JSON value. Object keys keep arrival order; duplicate keys keep the
// last value (Get returns the last match, like encoding/json).
struct Value {
  enum class Type { kNull, kBool, kNumber, kString, kObject, kArray };

  Type type = Type::kNull;
  bool boolean = false;
  double number = 0.0;
  std::string str;
  // Object members, in arrival order. A dedicated struct rather than
  // std::pair<std::string, Value>: pair instantiates its type traits against the
  // still-incomplete Value (libstdc++ 16 rejects that outright), while
  // vector<Member> with Member merely forward-declared is guaranteed to work
  // since C++17. Field names keep pair's, so no call site moves.
  struct Member;
  std::vector<Member> object;
  std::vector<Value> array;

  bool IsObject() const { return type == Type::kObject; }
  // Object member lookup; nullptr when absent or not an object.
  const Value* Get(std::string_view key) const;
  // Typed accessors with defaults for absent/mistyped values.
  std::string GetString(std::string_view key, std::string_view def = "") const;
  double GetNumber(std::string_view key, double def = 0.0) const;
  int64_t GetInt(std::string_view key, int64_t def = 0) const;
};

struct Value::Member {
  std::string first;
  Value second;
};

// Parses exactly one JSON value (plus surrounding whitespace) from text.
// Returns false on malformed input or nesting deeper than an internal cap.
bool Parse(std::string_view text, Value* out);

}  // namespace json
}  // namespace birdman

#endif  // BIRDMAN_SRC_JSON_H_
