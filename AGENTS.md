# Agent guidance

For user-approved Local Agent Harness onboarding, follow [the canonical setup playbook](docs/AGENT_SETUP.md). Keep discovery limited to exact sources the user selected. Never scan credential stores, `.env` files, private keys, or broad home-directory paths; never ask the model to read or return secret values. Treat client registration and configuration checks as separate from an actual successful MCP read.
