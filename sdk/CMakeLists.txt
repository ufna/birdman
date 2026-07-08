# birdman SDK — C++17 server-side liba + example game.
#
#   cmake -S sdk -B sdk/build -DCMAKE_BUILD_TYPE=Release
#   cmake --build sdk/build -j
#   ctest --test-dir sdk/build --output-on-failure
#
# Options:
#   -DBIRDMAN_SANITIZE=thread|address   sanitized test build (ctest under TSAN/ASAN)
cmake_minimum_required(VERSION 3.16)
project(birdman_sdk CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)
if(NOT CMAKE_BUILD_TYPE AND NOT CMAKE_CONFIGURATION_TYPES)
  set(CMAKE_BUILD_TYPE RelWithDebInfo)
endif()

set(BIRDMAN_SANITIZE "" CACHE STRING "Sanitizer: thread | address | (empty)")
if(BIRDMAN_SANITIZE STREQUAL "thread")
  add_compile_options(-fsanitize=thread -g -fno-omit-frame-pointer)
  add_link_options(-fsanitize=thread)
elseif(BIRDMAN_SANITIZE STREQUAL "address")
  add_compile_options(-fsanitize=address,undefined -g -fno-omit-frame-pointer)
  add_link_options(-fsanitize=address,undefined)
endif()

enable_testing()

add_subdirectory(core)
