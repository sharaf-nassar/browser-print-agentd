This directory defines the high-level concepts, business logic, and architecture of this project using markdown. It is managed by [lat.md](https://www.npmjs.com/package/lat.md) — a tool that anchors source code to these definitions. Install the `lat` command with `npm i -g lat.md` and run `lat --help`.

- [[tools]] — the daemon: the CUPS coupling point, stable printer identity, health-gated failover, the origin posture, and the version surface.
- [[packaging]] — the single identity source, the rendered `.in` template chain, and what the station installer does to a Mac.
- [[infrastructure]] — the naming gate and the contract any release path is held to.
- [[tests]] — what each test in the tree is responsible for proving.
