<!-- AI-GUIDELINES:BEGIN -->
## AI Engineering Guidelines

Read `.agents/general.md` and `.agents/preferences.md` for every task. Then select the smallest matching context profile below. Do not read every installed companion by default. Precedence follows `.agents/general.md` §2: repository-local instructions win, then the `haikel-*` owner profile, then the other guidelines.

- **Repository setup, documentation, planning, CI, or process:** `.agents/harness.md`.
- **Go language, package, or service work:** `.agents/guidelines/go.md` and `.agents/guidelines/haikel-go.md`.
- **Container, Compose, delivery, or runtime work:** `.agents/guidelines/docker.md`; add an application profile only when its build or runtime behavior also changes.
- **Cross-cutting work:** use the union of only the affected profiles and state why each additional document is needed.
<!-- AI-GUIDELINES:END -->
