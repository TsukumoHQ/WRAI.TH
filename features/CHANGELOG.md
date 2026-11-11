# Changelog — auto-assembled by the niwa scribe at merge

## [relay/identity] a host stall of a few seconds never fails an authed call, and the relay stops adding its own multi-second integrity scans to the load

Identity verification now retries once on timeout and caches results for 3 seconds, preventing authentication failures during host stalls.

```mermaid
flowchart TD
    A[Identity Check] --> B{Cache Hit?}
    B -->|Yes, TTL Valid| C[Return Cached]
    B -->|No/Expired| D[Query Database]
    D --> E{Timeout?}
    E -->|Yes| F[Retry Once]
    F --> G{Success?}
    G -->|Yes| H[Cache & Return]
    G -->|No| I[Return Error]
    E -->|No| H
    C --> J[Allow Access]
    H --> J
    I --> K[Deny Access]
```

