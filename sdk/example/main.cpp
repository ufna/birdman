// birdman SDK example "game": a UDP chat-lite server wired to the full
// birdman lifecycle through sdk/core — the C++ twin of examples/stub-server.
// Without BIRDMAN_SOCKET it runs standalone (SDK no-op mode): same binary,
// no agent, zero errors.
//
// UDP protocol (text, one datagram = one command):
//   PING          -> PONG server=<id> players=<n> state=<ready|allocated|match|draining>
//   JOIN <name>   -> WELCOME <name> players=<n>
//   LEAVE         -> BYE <name>
//   anything else -> ECHO <input>
//
// Lifecycle (docs/specs/sdk.md §2): NotifyReady after the port is bound;
// match starts when a match is allocated AND a player is in; match ends when
// the last player leaves -> NotifyMatchEnd -> exit 0 (one-shot dedicated
// server). Drain: finish the current match within the deadline, never start
// a new one. Uses CallbackMode::kPoll: events are handled on the main loop,
// so this file needs no locks at all.
//
// `--client <port>` is a tiny smoke-test client (JOIN -> WELCOME -> LEAVE ->
// BYE) used by sdk/scripts/smoke.sh.
#include <arpa/inet.h>
#include <netinet/in.h>
#include <poll.h>
#include <sys/socket.h>
#include <unistd.h>

#include <chrono>
#include <cstdio>
#include <cstring>
#include <map>
#include <string>

#include "birdman/birdman.h"

namespace {

using Clock = std::chrono::steady_clock;

struct Game {
  birdman::ServerLink link;
  bool managed = false;
  bool allocated = false;
  bool match_live = false;
  bool draining = false;
  Clock::time_point drain_deadline{};
  std::map<std::string, std::string> players;  // "ip:port" -> name

  const char* State() const {
    if (draining) return "draining";
    if (match_live) return "match";
    if (allocated) return "allocated";
    return "ready";
  }
};

std::string PeerKey(const sockaddr_in& a) {
  char ip[INET_ADDRSTRLEN] = {0};
  inet_ntop(AF_INET, &a.sin_addr, ip, sizeof(ip));
  return std::string(ip) + ":" + std::to_string(ntohs(a.sin_port));
}

void Reply(int fd, const sockaddr_in& to, const std::string& msg) {
  std::string line = msg + "\n";
  sendto(fd, line.data(), line.size(), 0, reinterpret_cast<const sockaddr*>(&to), sizeof(to));
}

void HandlePacket(Game* g, int fd, const sockaddr_in& from, std::string msg) {
  while (!msg.empty() && (msg.back() == '\n' || msg.back() == '\r')) msg.pop_back();
  const std::string key = PeerKey(from);
  if (msg.rfind("PING", 0) == 0) {
    Reply(fd, from,
          "PONG server=" + g->link.ServerId() + " players=" + std::to_string(g->players.size()) +
              " state=" + g->State());
  } else if (msg.rfind("JOIN", 0) == 0) {
    if (g->draining) {
      Reply(fd, from, "ERR draining, no new players");
      return;
    }
    std::string name = msg.size() > 5 ? msg.substr(5) : "anon";
    g->players[key] = name;
    g->link.SetPlayerCount(static_cast<int>(g->players.size()));
    Reply(fd, from, "WELCOME " + name + " players=" + std::to_string(g->players.size()));
  } else if (msg.rfind("LEAVE", 0) == 0) {
    auto it = g->players.find(key);
    if (it != g->players.end()) {
      Reply(fd, from, "BYE " + it->second);
      g->players.erase(it);
      g->link.SetPlayerCount(static_cast<int>(g->players.size()));
    }
  } else {
    Reply(fd, from, "ECHO " + msg);
  }
}

int RunServer() {
  Game g;
  birdman::Config cfg;
  cfg.callback_mode = birdman::CallbackMode::kPoll;  // events on the main loop
  cfg.on_allocated = [&g](const birdman::AllocatedEvent& ev) {
    std::printf("[example] allocated: match_id=%s players_expected=%d\n", ev.match_id.c_str(),
                ev.players_expected);
    g.allocated = true;
  };
  cfg.on_drain_requested = [&g](const birdman::DrainEvent& ev) {
    std::printf("[example] drain requested: deadline=%.0fs reason=%s\n", ev.deadline_seconds,
                ev.reason.c_str());
    g.draining = true;
    g.drain_deadline =
        Clock::now() + std::chrono::milliseconds(static_cast<long>(ev.deadline_seconds * 1000));
  };
  g.managed = g.link.Init(cfg);

  const int port = g.link.Port() != 0 ? g.link.Port() : 7777;
  int fd = socket(AF_INET, SOCK_DGRAM, 0);
  if (fd < 0) {
    std::perror("socket");
    return 1;
  }
  sockaddr_in addr{};
  addr.sin_family = AF_INET;
  addr.sin_addr.s_addr = htonl(INADDR_ANY);
  addr.sin_port = htons(static_cast<uint16_t>(port));
  if (bind(fd, reinterpret_cast<sockaddr*>(&addr), sizeof(addr)) != 0) {
    std::perror("bind");
    return 1;
  }
  std::printf("[example] server_id=%s udp=:%d managed=%s\n", g.link.ServerId().c_str(), port,
              g.managed ? "yes" : "no (standalone)");

  g.link.NotifyReady();  // port bound, "map loaded" — ready for a match

  auto next_metric = Clock::now() + std::chrono::seconds(5);
  while (true) {
    g.link.PollCallbacks();  // on_allocated / on_drain_requested run here

    if (g.allocated && !g.match_live && !g.players.empty()) {
      g.match_live = true;
      std::printf("[example] match %s started\n", g.link.MatchId().c_str());
      g.link.NotifyMatchStart();
    }
    if (g.match_live && g.players.empty()) {
      g.match_live = false;
      std::printf("[example] match %s ended: completed\n", g.link.MatchId().c_str());
      g.link.NotifyMatchEnd(birdman::MatchResult::kCompleted);
      if (g.managed) break;  // one-shot dedicated server: exit, fleet recreates
      g.allocated = false;
    }
    if (g.draining && !g.match_live) {
      std::printf("[example] drained, exiting\n");
      break;
    }
    if (g.draining && g.match_live && Clock::now() >= g.drain_deadline) {
      std::printf("[example] drain deadline hit, aborting match\n");
      g.link.NotifyMatchEnd(birdman::MatchResult::kAborted);
      break;
    }
    if (Clock::now() >= next_metric) {
      g.link.ReportMetric("tick_ms", 16.6);  // a real game reports its frame time
      next_metric = Clock::now() + std::chrono::seconds(5);
    }

    pollfd pfd{fd, POLLIN, 0};
    if (poll(&pfd, 1, 100) > 0 && (pfd.revents & POLLIN) != 0) {
      char buf[2048];
      sockaddr_in from{};
      socklen_t from_len = sizeof(from);
      ssize_t n =
          recvfrom(fd, buf, sizeof(buf) - 1, 0, reinterpret_cast<sockaddr*>(&from), &from_len);
      if (n > 0) HandlePacket(&g, fd, from, std::string(buf, static_cast<size_t>(n)));
    }
  }

  g.link.Shutdown();
  close(fd);
  return 0;
}

// Minimal smoke-test client: JOIN -> WELCOME, LEAVE -> BYE.
int RunClient(int port) {
  int fd = socket(AF_INET, SOCK_DGRAM, 0);
  sockaddr_in to{};
  to.sin_family = AF_INET;
  to.sin_port = htons(static_cast<uint16_t>(port));
  inet_pton(AF_INET, "127.0.0.1", &to.sin_addr);

  auto rpc = [&](const std::string& cmd, const char* want) -> bool {
    for (int attempt = 0; attempt < 20; ++attempt) {  // UDP: retry politely
      sendto(fd, cmd.data(), cmd.size(), 0, reinterpret_cast<sockaddr*>(&to), sizeof(to));
      pollfd pfd{fd, POLLIN, 0};
      if (poll(&pfd, 1, 500) <= 0) continue;
      char buf[2048];
      ssize_t n = recv(fd, buf, sizeof(buf) - 1, 0);
      if (n <= 0) continue;
      buf[n] = '\0';
      std::printf("[client] %s -> %s", cmd.c_str(), buf);
      if (std::strncmp(buf, want, std::strlen(want)) == 0) return true;
    }
    std::fprintf(stderr, "[client] no %s reply for %s\n", want, cmd.c_str());
    return false;
  };

  bool ok = rpc("JOIN smoke", "WELCOME") && rpc("LEAVE", "BYE");
  close(fd);
  return ok ? 0 : 1;
}

}  // namespace

int main(int argc, char** argv) {
  // Dedicated servers log through a pipe (containerd) — keep stdout
  // line-buffered so the platform sees log lines as they happen.
  setvbuf(stdout, nullptr, _IOLBF, 0);
  if (argc >= 3 && std::strcmp(argv[1], "--client") == 0) {
    return RunClient(std::atoi(argv[2]));
  }
  return RunServer();
}
