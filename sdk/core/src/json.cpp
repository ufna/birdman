#include "json.h"

#include <charconv>
#include <cmath>
#include <cstdio>
#include <cstdlib>
#include <cstring>

namespace birdman {
namespace json {

namespace {
constexpr int kMaxDepth = 64;
}  // namespace

// ---- writer ----------------------------------------------------------------

void AppendString(std::string* out, std::string_view s) {
  out->push_back('"');
  for (unsigned char c : s) {
    switch (c) {
      case '"':
        *out += "\\\"";
        break;
      case '\\':
        *out += "\\\\";
        break;
      case '\b':
        *out += "\\b";
        break;
      case '\f':
        *out += "\\f";
        break;
      case '\n':
        *out += "\\n";
        break;
      case '\r':
        *out += "\\r";
        break;
      case '\t':
        *out += "\\t";
        break;
      default:
        if (c < 0x20) {
          char buf[8];
          std::snprintf(buf, sizeof(buf), "\\u%04x", c);
          *out += buf;
        } else {
          out->push_back(static_cast<char>(c));
        }
    }
  }
  out->push_back('"');
}

void AppendNumber(std::string* out, double v) {
  if (std::isnan(v) || std::isinf(v)) {  // not representable in JSON
    out->push_back('0');
    return;
  }
  double integral = 0.0;
  if (std::modf(v, &integral) == 0.0 && std::fabs(v) < 1e15) {
    // Whole values print as integers ("2", not "2.0") — matches Go's
    // encoding/json, keeps golden frames tidy.
    AppendNumber(out, static_cast<int64_t>(integral));
    return;
  }
  char buf[64];
#if defined(__cpp_lib_to_chars) && __cpp_lib_to_chars >= 201611L
  auto res = std::to_chars(buf, buf + sizeof(buf), v);
  out->append(buf, res.ptr);
#else
  std::snprintf(buf, sizeof(buf), "%.17g", v);
  out->append(buf);
#endif
}

void AppendNumber(std::string* out, int64_t v) {
  char buf[32];
  auto res = std::to_chars(buf, buf + sizeof(buf), v);
  out->append(buf, res.ptr);
}

void Obj::Key(std::string_view key) {
  if (!first_) buf_.push_back(',');
  first_ = false;
  AppendString(&buf_, key);
  buf_.push_back(':');
}

Obj& Obj::Str(std::string_view key, std::string_view value) {
  Key(key);
  AppendString(&buf_, value);
  return *this;
}

Obj& Obj::Int(std::string_view key, int64_t value) {
  Key(key);
  AppendNumber(&buf_, value);
  return *this;
}

Obj& Obj::Num(std::string_view key, double value) {
  Key(key);
  AppendNumber(&buf_, value);
  return *this;
}

Obj& Obj::Raw(std::string_view key, std::string_view json_value) {
  Key(key);
  buf_.append(json_value);
  return *this;
}

std::string Obj::Done() {
  buf_.push_back('}');
  return std::move(buf_);
}

std::string Frame(std::string_view type, std::string_view data_json) {
  std::string out = "{\"v\":1,\"type\":";
  AppendString(&out, type);
  out += ",\"data\":";
  out.append(data_json);
  out += "}\n";
  return out;
}

// ---- reader ----------------------------------------------------------------

const Value* Value::Get(std::string_view key) const {
  if (type != Type::kObject) return nullptr;
  const Value* found = nullptr;
  for (const auto& kv : object) {
    if (kv.first == key) found = &kv.second;  // last duplicate wins
  }
  return found;
}

std::string Value::GetString(std::string_view key, std::string_view def) const {
  const Value* v = Get(key);
  if (v == nullptr || v->type != Type::kString) return std::string(def);
  return v->str;
}

double Value::GetNumber(std::string_view key, double def) const {
  const Value* v = Get(key);
  if (v == nullptr || v->type != Type::kNumber) return def;
  return v->number;
}

int64_t Value::GetInt(std::string_view key, int64_t def) const {
  const Value* v = Get(key);
  if (v == nullptr || v->type != Type::kNumber) return def;
  return static_cast<int64_t>(v->number);
}

namespace {

class Parser {
 public:
  explicit Parser(std::string_view text) : p_(text.data()), end_(text.data() + text.size()) {}

  bool Run(Value* out) {
    SkipWs();
    if (!ParseValue(out, 0)) return false;
    SkipWs();
    return p_ == end_;  // trailing garbage = malformed
  }

 private:
  void SkipWs() {
    while (p_ != end_ && (*p_ == ' ' || *p_ == '\t' || *p_ == '\n' || *p_ == '\r')) ++p_;
  }

  bool Eat(char c) {
    if (p_ != end_ && *p_ == c) {
      ++p_;
      return true;
    }
    return false;
  }

  bool Literal(std::string_view lit) {
    if (static_cast<size_t>(end_ - p_) < lit.size()) return false;
    if (std::memcmp(p_, lit.data(), lit.size()) != 0) return false;
    p_ += lit.size();
    return true;
  }

  bool ParseValue(Value* out, int depth) {
    if (depth > kMaxDepth || p_ == end_) return false;
    switch (*p_) {
      case '{':
        return ParseObject(out, depth);
      case '[':
        return ParseArray(out, depth);
      case '"':
        out->type = Value::Type::kString;
        return ParseString(&out->str);
      case 't':
        out->type = Value::Type::kBool;
        out->boolean = true;
        return Literal("true");
      case 'f':
        out->type = Value::Type::kBool;
        out->boolean = false;
        return Literal("false");
      case 'n':
        out->type = Value::Type::kNull;
        return Literal("null");
      default:
        return ParseNumber(out);
    }
  }

  bool ParseObject(Value* out, int depth) {
    out->type = Value::Type::kObject;
    ++p_;  // '{'
    SkipWs();
    if (Eat('}')) return true;
    for (;;) {
      SkipWs();
      std::string key;
      if (p_ == end_ || *p_ != '"' || !ParseString(&key)) return false;
      SkipWs();
      if (!Eat(':')) return false;
      SkipWs();
      Value member;
      if (!ParseValue(&member, depth + 1)) return false;
      // Braced, not emplace_back(k, v): Member is an aggregate and parenthesised
      // aggregate init inside emplace is C++20 -- core is C++17.
      out->object.push_back(Value::Member{std::move(key), std::move(member)});
      SkipWs();
      if (Eat(',')) continue;
      return Eat('}');
    }
  }

  bool ParseArray(Value* out, int depth) {
    out->type = Value::Type::kArray;
    ++p_;  // '['
    SkipWs();
    if (Eat(']')) return true;
    for (;;) {
      SkipWs();
      Value elem;
      if (!ParseValue(&elem, depth + 1)) return false;
      out->array.push_back(std::move(elem));
      SkipWs();
      if (Eat(',')) continue;
      return Eat(']');
    }
  }

  static void AppendUtf8(std::string* out, uint32_t cp) {
    if (cp < 0x80) {
      out->push_back(static_cast<char>(cp));
    } else if (cp < 0x800) {
      out->push_back(static_cast<char>(0xC0 | (cp >> 6)));
      out->push_back(static_cast<char>(0x80 | (cp & 0x3F)));
    } else if (cp < 0x10000) {
      out->push_back(static_cast<char>(0xE0 | (cp >> 12)));
      out->push_back(static_cast<char>(0x80 | ((cp >> 6) & 0x3F)));
      out->push_back(static_cast<char>(0x80 | (cp & 0x3F)));
    } else {
      out->push_back(static_cast<char>(0xF0 | (cp >> 18)));
      out->push_back(static_cast<char>(0x80 | ((cp >> 12) & 0x3F)));
      out->push_back(static_cast<char>(0x80 | ((cp >> 6) & 0x3F)));
      out->push_back(static_cast<char>(0x80 | (cp & 0x3F)));
    }
  }

  bool ParseHex4(uint32_t* out) {
    if (end_ - p_ < 4) return false;
    uint32_t v = 0;
    for (int i = 0; i < 4; ++i) {
      char c = *p_++;
      v <<= 4;
      if (c >= '0' && c <= '9') {
        v |= static_cast<uint32_t>(c - '0');
      } else if (c >= 'a' && c <= 'f') {
        v |= static_cast<uint32_t>(c - 'a' + 10);
      } else if (c >= 'A' && c <= 'F') {
        v |= static_cast<uint32_t>(c - 'A' + 10);
      } else {
        return false;
      }
    }
    *out = v;
    return true;
  }

  bool ParseString(std::string* out) {
    ++p_;  // '"'
    for (;;) {
      if (p_ == end_) return false;
      unsigned char c = static_cast<unsigned char>(*p_);
      if (c == '"') {
        ++p_;
        return true;
      }
      if (c == '\\') {
        ++p_;
        if (p_ == end_) return false;
        char esc = *p_++;
        switch (esc) {
          case '"':
            out->push_back('"');
            break;
          case '\\':
            out->push_back('\\');
            break;
          case '/':
            out->push_back('/');
            break;
          case 'b':
            out->push_back('\b');
            break;
          case 'f':
            out->push_back('\f');
            break;
          case 'n':
            out->push_back('\n');
            break;
          case 'r':
            out->push_back('\r');
            break;
          case 't':
            out->push_back('\t');
            break;
          case 'u': {
            uint32_t cp = 0;
            if (!ParseHex4(&cp)) return false;
            if (cp >= 0xD800 && cp <= 0xDBFF) {  // high surrogate
              uint32_t lo = 0;
              if (!Literal("\\u") || !ParseHex4(&lo)) return false;
              if (lo < 0xDC00 || lo > 0xDFFF) return false;
              cp = 0x10000 + ((cp - 0xD800) << 10) + (lo - 0xDC00);
            } else if (cp >= 0xDC00 && cp <= 0xDFFF) {
              return false;  // lone low surrogate
            }
            AppendUtf8(out, cp);
            break;
          }
          default:
            return false;
        }
        continue;
      }
      if (c < 0x20) return false;  // raw control char
      out->push_back(static_cast<char>(c));
      ++p_;
    }
  }

  bool ParseNumber(Value* out) {
    const char* start = p_;
    if (p_ != end_ && *p_ == '-') ++p_;
    while (p_ != end_ &&
           ((*p_ >= '0' && *p_ <= '9') || *p_ == '+' || *p_ == '-' || *p_ == '.' || *p_ == 'e' ||
            *p_ == 'E')) {
      ++p_;
    }
    if (p_ == start) return false;
    // strtod needs a NUL-terminated buffer; numbers are short.
    char buf[64];
    size_t len = static_cast<size_t>(p_ - start);
    if (len >= sizeof(buf)) return false;
    std::memcpy(buf, start, len);
    buf[len] = '\0';
    char* parse_end = nullptr;
    double v = std::strtod(buf, &parse_end);
    if (parse_end != buf + len) return false;
    out->type = Value::Type::kNumber;
    out->number = v;
    return true;
  }

  const char* p_;
  const char* end_;
};

}  // namespace

bool Parse(std::string_view text, Value* out) {
  *out = Value{};
  Parser parser(text);
  return parser.Run(out);
}

}  // namespace json
}  // namespace birdman
