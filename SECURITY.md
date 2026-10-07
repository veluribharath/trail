# Security

trail shows agent transcripts, which often contain secrets: API keys in command output, file contents, environment details. Its defaults reflect that. It listens on 127.0.0.1 only, refuses cross-site requests to its API, loads nothing from the network, and only opens files its own discovery found.

If you find a way around any of that, for example reading a file outside the agents' session folders, or another site reading trail's API, please report it privately through GitHub's **Report a vulnerability** button on the repository's Security tab rather than in a public issue.

Running with `-host 0.0.0.0` exposes every transcript to anyone who can reach the port. That is documented behavior, not a vulnerability.
