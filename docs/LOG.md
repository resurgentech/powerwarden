# Work log

Append-only history of what shipped from `ROADMAP.md`. FOREMAN appends
an entry per landed step or tranche. Newest at the bottom. Corrections
are appended, never edited in; a hook refuses rewrites.

```
## YYYY-MM-DD — <step id and title>

- What shipped
- Commits: <shas>
```

---

## 2026-11-18 — initial import

- powermaster (`powermaster/{api,daemon}`), powerwarden (`powerwarden/{api,daemon,mcp}`), their OpenAPI specs
  and design docs imported as five Go modules under `github.com/resurgentech/powerwarden/`, joined by a
  committed `go.work`; each module tagged `<dir>/v0.1.0`. orch process files seeded.
- Commits: the initial commit.
