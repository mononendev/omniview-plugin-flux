# Flux CD plugin

Flux CD tools and views for Kubernetes connections in Omniview.

## 0.5.0 compatibility

Flux 0.5.0 requires Omniview 0.36.0 or later and the compatible Kubernetes plugin from the coordinated release (0.21.0). Upgrade Omniview, Kubernetes, and Flux together. Flux 0.4.2 uses the previous host callback protocol and is not compatible with this cutover; Flux 0.5.0 does not support older hosts.

This release targets SDK v0.20.0 and runtime/Vite plugin 0.36.0. Schema resolution now selects the exact Kubernetes connection, resource type, and catalog representation ID and revision. Referenced schema documents retain that same target, exact document reference, dialect, and local schema root. Schema denial or unavailable schemas do not enable a fallback.

Flux remains a tools/UI plugin, not a resource provider. Its Kubernetes owner grants remain limited to catalog, get, enumerate, and update on the existing five Flux API-group patterns. Schema resolution uses the existing catalog grant; it does not require broader permissions or direct Kubernetes credentials.

## Coordinated release

Version 0.5.0 is release preparation, not a publication receipt. Before the upstream maintainer releases it:

- Publish SDK v0.20.0 and runtime/Vite plugin 0.36.0, then refresh `go.sum` and `ui/pnpm-lock.yaml` from those public packages. Keep the existing grpc-go fork replacement.
- Validate the Go backend, UI, and canonical plugin archives against the coordinated host and Kubernetes release. Exercise schema denial and target isolation, status, reconcile, suspend/resume, controller discovery, and complete live collections.
- Release through the upstream maintainer and existing `mononen` publisher as plugin `flux`, with minimum IDE version 0.36.0. A fork or pull request is not a compatible marketplace release.

The existing publish workflow retains its configured marketplace API and publisher identity. Its minimum IDE version is 0.36.0.
