# birdman_core — static C++17 library, POSIX + standard library only.
add_library(birdman_core STATIC
  src/json.cpp
  src/link.cpp
)
target_include_directories(birdman_core PUBLIC
  $<BUILD_INTERFACE:${CMAKE_CURRENT_SOURCE_DIR}/include>
)
target_compile_options(birdman_core PRIVATE -Wall -Wextra)

find_package(Threads REQUIRED)
target_link_libraries(birdman_core PUBLIC Threads::Threads)

# ---- tests -------------------------------------------------------------------

add_executable(birdman_core_tests
  tests/main.cpp
  tests/json_test.cpp
  tests/golden_test.cpp
  tests/link_test.cpp
)
target_link_libraries(birdman_core_tests PRIVATE birdman_core)
target_include_directories(birdman_core_tests PRIVATE src)
target_compile_options(birdman_core_tests PRIVATE -Wall -Wextra)

# Keep in sync with the BT_TEST names (birdman_core_tests --list).
set(BIRDMAN_CORE_TESTS
  json_writer
  json_parser
  golden_outgoing
  golden_incoming
  noop_mode
  lifecycle
  ping_pong
  ping_pong_after_idle
  drain_callback
  poll_mode
  reconnect_replay
  ring_buffer
  ring_overflow
  metric_rate_limit
  concurrency
)
foreach(t IN LISTS BIRDMAN_CORE_TESTS)
  add_test(NAME core.${t} COMMAND birdman_core_tests ${t})
  set_tests_properties(core.${t} PROPERTIES TIMEOUT 60)
endforeach()

# Guard against BT_TEST/CMake list drift: the binary exits non-zero if a
# registered test is missing from the list above (and vice versa).
list(JOIN BIRDMAN_CORE_TESTS "," _birdman_core_tests_joined)
add_test(NAME core.registry_sync
         COMMAND birdman_core_tests --verify-list ${_birdman_core_tests_joined})
set_tests_properties(core.registry_sync PROPERTIES TIMEOUT 30)
