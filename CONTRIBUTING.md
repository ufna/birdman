# Contributing

birdman was built for our own games and then open-sourced. It is young software
shaped around our needs, so before investing time in a large change, open an
issue and let's agree on the shape — a rejected big PR wastes your evening, not
ours.

Small fixes need no ceremony: open the PR.

## Before you open a PR

```bash
./check.sh
```

That is the same set of checks CI runs, minus what needs Docker, Postgres,
ansible or a TSAN toolchain. It skips loudly whatever your machine cannot run.

Then read **[AGENTS.md](AGENTS.md)** — it is written for coding agents but is
exactly what a human needs too: the repository layout, per-component commands,
and the handful of rules that are not visible from the code (the panel is
strictly bilingual, docs are written in English, `openapi.yaml` is generated, a new
endpoint is one entry in the route table).

## Commits

Conventional-commit prefix, Russian subject line, matching the existing history:

```
feat(panel): демо-ряды метрик — сутки с формой в любом окне
fix(master): дренящаяся нода больше не считается активной
```

English subjects are accepted too — do not let the language stop you from
sending a fix. Consistency of the prefix matters more than the language of the
sentence after it.

## What makes a change easy to accept

- **It explains why.** Comments in this codebase carry reasons, not restatements
  of the code. A change that only says *what* it does will be asked *why*.
- **It comes with the test that would have caught the bug.** Not coverage for
  its own sake — the specific pin that fails before the fix and passes after.
- **It keeps the single sources of truth single.** The API surface lives in the
  route table; the contract is generated from it. A second copy of a fact
  anywhere is the defect we care most about avoiding.
- **It does not reintroduce Kubernetes.** Running a fleet without it is the
  point of the project.

## What we are unlikely to take

- A rewrite of a subsystem without a prior issue agreeing on it.
- Support for platforms other than Linux on the node side.
- New runtime dependencies in the master, unless they earn their place loudly.
- Features whose only justification is that another product has them.

## Reporting a security issue

Do not open an issue. See [SECURITY.md](SECURITY.md).

## License

By contributing, you agree that your contribution is licensed under the
[MIT License](LICENSE) that covers this project.
