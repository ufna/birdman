# Example game on top of birdman_core; also the smoke-test subject
# (sdk/scripts/smoke.sh drives it against sdk/mockagent).
add_executable(birdman_example main.cpp)
target_link_libraries(birdman_example PRIVATE birdman_core)
target_compile_options(birdman_example PRIVATE -Wall -Wextra)

# App (game) version baked into the binary and surfaced in PING/PONG and the
# startup log — distinct from the frozen SDK wire version. The Dockerfile wires
# this to the image tag so a soft deploy is observable over UDP. Defaults to
# "dev" for local builds.
set(BIRDMAN_EXAMPLE_VERSION "dev" CACHE STRING "example game version baked into the binary")
target_compile_definitions(birdman_example PRIVATE
  BIRDMAN_EXAMPLE_VERSION="${BIRDMAN_EXAMPLE_VERSION}")
