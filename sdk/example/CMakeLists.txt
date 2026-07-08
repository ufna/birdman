# Example game on top of birdman_core; also the smoke-test subject
# (sdk/scripts/smoke.sh drives it against sdk/mockagent).
add_executable(birdman_example main.cpp)
target_link_libraries(birdman_example PRIVATE birdman_core)
target_compile_options(birdman_example PRIVATE -Wall -Wextra)
