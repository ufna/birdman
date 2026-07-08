// Micro test framework (no external deps): BT_TEST registers a test, BT_CHECK*
// throw on failure, main.cpp runs one test by name (ctest) or all of them.
#ifndef BIRDMAN_TESTS_FRAMEWORK_H_
#define BIRDMAN_TESTS_FRAMEWORK_H_

#include <map>
#include <sstream>
#include <stdexcept>
#include <string>

namespace bt {

struct Failure : std::runtime_error {
  using std::runtime_error::runtime_error;
};

using TestFn = void (*)();

inline std::map<std::string, TestFn>& Registry() {
  static std::map<std::string, TestFn> reg;
  return reg;
}

struct Register {
  Register(const char* name, TestFn fn) { Registry()[name] = fn; }
};

[[noreturn]] inline void FailAt(const char* file, int line, const std::string& msg) {
  std::ostringstream os;
  os << file << ":" << line << ": " << msg;
  throw Failure(os.str());
}

template <typename A, typename B>
[[noreturn]] void FailEq(const char* file, int line, const char* expr_a, const char* expr_b,
                         const A& a, const B& b) {
  std::ostringstream os;
  os << "CHECK_EQ(" << expr_a << ", " << expr_b << ") failed: " << a << " != " << b;
  FailAt(file, line, os.str());
}

}  // namespace bt

#define BT_TEST(name)                                             \
  static void bt_test_##name();                                   \
  static ::bt::Register bt_reg_##name(#name, &bt_test_##name);    \
  static void bt_test_##name()

#define BT_CHECK(cond)                                                    \
  do {                                                                    \
    if (!(cond)) ::bt::FailAt(__FILE__, __LINE__, "CHECK failed: " #cond); \
  } while (0)

#define BT_CHECK_EQ(a, b)                                       \
  do {                                                          \
    const auto& bt_a = (a);                                     \
    const auto& bt_b = (b);                                     \
    if (!(bt_a == bt_b)) ::bt::FailEq(__FILE__, __LINE__, #a, #b, bt_a, bt_b); \
  } while (0)

#define BT_FAIL(msg) ::bt::FailAt(__FILE__, __LINE__, (msg))

#endif  // BIRDMAN_TESTS_FRAMEWORK_H_
