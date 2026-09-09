# ChatGPT backend passthrough

Native authenticated passthrough for ChatGPT's `/backend-api/` HTTP surface.
The host authenticates every request, streams request and response bodies, and
keeps stored credential secrets opaque to the plugin.

Requires host plugin schema 8. Older hosts reject the library rather than
silently ignoring its ingress capability. Configure a frontend API key before
enabling this plugin.

The credential selector uses the host's shared active credential pool. It does
not restrict credentials by frontend principal. The oldest active Codex
credential whose first non-empty configured account identity matches
`ChatGPT-Account-ID` is used; otherwise the inbound `Authorization` value is
retained.

```yaml
plugins:
  configs:
    chatgpt-backend:
      base-url: https://chatgpt.com
```

Build with the release version injected into the shared native helper:

```bash
go build -buildmode=c-shared \
  -ldflags '-X github.com/unstableneutron/cpa-plugins/internal/nativeabi.Version=VERSION' \
  -o chatgpt-backend.so ./chatgpt-backend
```

Parity source: `unstableneutron/CLIProxyAPIPlus` commit
`1fec8453e63a5bc133555a79164480700e351bfc`, especially
`internal/api/chatgpt_backend_passthrough.go` and its tests.
