# Node service resource baseline

Measured before the Go migration, as the reference for its resource targets. Commit `6a448a8`, Node 24.22.3, Pi 1.5.2, Linux x64 (8 vCPU Intel Xeon, 31 GiB RAM), `node apps/api/src/main.ts` with a fresh hosted data directory and the default `process` work computer.

| Measure | Result |
| --- | --- |
| Idle RSS, 30 s after start, no active Bot | 175–183 MB, 1 process |
| RSS with one active Bot, turn in progress | 445 MB total: service 196 MB, Persona worker 108 MB, `pi --mode rpc` 140 MB |
| RSS after the turn settles | 149 MB, 1 process (worker and Pi exit) |
| Cold start to first `200 /health` | 855 ms median of 10 warm runs (799–1186 ms); 829 ms on a fresh data directory |
| `deploy/Dockerfile` app image | 2.46 GB on disk, 668 MB compressed |
| `deploy/workspace.Dockerfile` work image | 2.55 GB on disk, 721 MB compressed |
| `deploy/proxy.Dockerfile` egress proxy image | 221 MB on disk, 48.8 MB compressed |

## Method

- RSS is the sum of `VmRSS` over the service process tree, so pages shared between processes count more than once.
- The active Bot used a local OpenAI-compatible mock model configured in Pi's `models.json` that streams one token per second for 40 s. The value is the peak of 25 one-second samples during the turn.
- The Bot was paired through `POST /hosting/desktop`, bound to the authorized `process` computer with automatic approval, set to the Pi engine, and sent one message through `send`.
- Image sizes come from `docker images` (Docker 29.9) after building each Dockerfile from the repository root with its default base images.
- Not measured: macOS and Windows, the Tauri desktop shell, Docker and controller work computers, and real provider latency.
