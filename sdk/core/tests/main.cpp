// Test runner: `birdman_core_tests <name>` runs one test (how ctest calls it),
// no args runs all, `--list` prints names, `--verify-list a,b,c` checks the
// CMake test list against the registry.
#include <csignal>
#include <cstdio>
#include <cstring>
#include <set>
#include <sstream>
#include <string>

#include "framework.h"

namespace {

int RunOne(const std::string& name) {
  auto it = bt::Registry().find(name);
  if (it == bt::Registry().end()) {
    std::fprintf(stderr, "unknown test: %s (see --list)\n", name.c_str());
    return 2;
  }
  try {
    it->second();
  } catch (const bt::Failure& f) {
    std::fprintf(stderr, "FAIL %s\n  %s\n", name.c_str(), f.what());
    return 1;
  } catch (const std::exception& e) {
    std::fprintf(stderr, "FAIL %s\n  unexpected exception: %s\n", name.c_str(), e.what());
    return 1;
  }
  std::fprintf(stderr, "PASS %s\n", name.c_str());
  return 0;
}

int VerifyList(const std::string& joined) {
  std::set<std::string> cmake_names;
  std::stringstream ss(joined);
  std::string item;
  while (std::getline(ss, item, ',')) {
    if (!item.empty()) cmake_names.insert(item);
  }
  int rc = 0;
  for (const auto& kv : bt::Registry()) {
    if (cmake_names.count(kv.first) == 0) {
      std::fprintf(stderr, "test %s is registered but missing from BIRDMAN_CORE_TESTS in CMakeLists.txt\n",
                   kv.first.c_str());
      rc = 1;
    }
  }
  for (const auto& name : cmake_names) {
    if (bt::Registry().count(name) == 0) {
      std::fprintf(stderr, "BIRDMAN_CORE_TESTS lists %s but no such BT_TEST exists\n", name.c_str());
      rc = 1;
    }
  }
  return rc;
}

}  // namespace

int main(int argc, char** argv) {
  std::signal(SIGPIPE, SIG_IGN);  // mock agent writes race liba disconnects

  if (argc > 1 && std::strcmp(argv[1], "--list") == 0) {
    for (const auto& kv : bt::Registry()) std::printf("%s\n", kv.first.c_str());
    return 0;
  }
  if (argc > 2 && std::strcmp(argv[1], "--verify-list") == 0) {
    return VerifyList(argv[2]);
  }
  if (argc > 1) {
    return RunOne(argv[1]);
  }
  int failed = 0;
  for (const auto& kv : bt::Registry()) {
    failed += RunOne(kv.first) != 0 ? 1 : 0;
  }
  if (failed != 0) {
    std::fprintf(stderr, "%d test(s) failed\n", failed);
    return 1;
  }
  return 0;
}
