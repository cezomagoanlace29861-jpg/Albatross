# A tester inside the country

The collector (`cmd/collect`, run by CI abroad) finds and tests thousands of
servers, but it tests from outside and cannot see the filtering users face.
This tester runs on a machine on an Iranian network, connects through the
candidate servers for real from there, twice, and reports which ones work.

Its report, `servers/iran.json` in the project's own storage, is read by the
collector (`-crowd`): servers confirmed from inside come first in `hot.txt`
and servers that failed there are left out; while the report is fresh (12
hours) and confirms at least 100 servers, `hot.txt` holds confirmed servers
only. The tester also publishes `hot.txt` itself after such a run, so the
apps get the list at once rather than at the next run abroad.

## Setting one up

A computer on an Iranian network, always on, with the internet an ordinary
user here has: **no VPN and no proxy** (the tester ignores proxy settings;
that network is what it measures). Ubuntu or Debian on x86-64 or ARM64,
including Ubuntu in WSL on a Windows PC. One command, as an ordinary user:

```sh
curl -fsSL https://accounts.albatrossvpn.online/list/tools/tester-install.sh | sudo bash
```

It asks for the account service's administrator (`backend/README.md`) and
keeps it on this machine only (root-only file); set `ADMIN_USER` and
`ADMIN_PASSWORD` to skip the questions. Everything it downloads comes from
the project's own storage: the collector built by CI and the cores
(sing-box, Xray); GitHub, or `CORES_URL`, only if those fail. It needs
nothing from apt on a base Ubuntu (curl, python3, tar, flock).

It runs at once and then every two hours (`EVERY=3h` changes that), and
connects through up to 600 servers a run (`REAL`). Each server that works
carries two small downloads, so a run can use several hundred MB.

- **WSL on Windows:** Windows starts it, hidden, whenever you sign in (a
  launcher in your Startup folder, no administrator rights needed), and the
  running tester keeps that Linux awake. Keep the PC on and awake.
- **A Linux machine with systemd:** a service that restarts by itself.
- **Otherwise:** a background loop, started again at boot by cron.

Its log: `sudo tail -f /opt/albatross-tester/log.txt`.

Several testers (different providers, mobile and fixed) can report in turn;
the latest report wins.
