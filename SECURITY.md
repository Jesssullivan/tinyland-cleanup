# Security

Report security-sensitive issues privately to the repository maintainer.

This daemon can remove local state, interact with build caches, and invoke
platform cleanup operations. Treat policy changes, privilege escalation paths,
service stop/start behavior, and offline compaction as security-sensitive.

Security-related pull requests should include:

- the affected platform or plugin;
- the safety invariant being preserved;
- dry-run output or equivalent evidence;
- validation commands that were run.
