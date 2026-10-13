# Backboard Provider

Backboard credentials grant access to AI models and conversations through CLIProxyAPI.

## Language

**Backboard credential**: An API key belonging to a Backboard account.

**Backboard model**: An AI model in Backboard's catalog, identified by its upstream provider and model name.

**Assistant**: A Backboard profile containing instructions and available tools. Its threads share that profile.

**Thread**: A Backboard conversation containing chronological messages and model responses.

**Run**: A model's response to a message, which may pause while waiting for tool results.

**Tool continuation**: Resuming a paused run by supplying results for all its pending tool calls.

**Model allowlist**: The selected Backboard models available to all credentials. An empty allowlist permits no models.

**Model alias**: A public request name assigned to a selected Backboard model.
