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

## [audit T5] compact boot payloads: discover_tools terse by default, re-register minimal, inbox budgeted

discover_tools returns compact signatures by default (name, description, required params) instead of full schemas to reduce boot payloads; get_inbox applies context budget constraints by default to prevent inbox overflow.

```mermaid
flowchart LR
    A["discover_tools()"] -->|default| B["Terse 1.5k"]
    A -->|verbose| C["Full schema"]
    D["get_inbox()"] -->|default| E["Pruned"]
    D -->|explicit| F["Full"]
```

