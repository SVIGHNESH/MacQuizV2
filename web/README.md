# MacQuiz web

React + TypeScript SPA for MacQuiz v2 (Vite).
UI work follows docs/11-frontend-design-system.md; all tokens live in `src/styles/tokens.css` and components never use raw hex.

## Commands

- `npm run dev` - dev server on :5173, proxies `/api` and `/healthz` to the Go API on :8080.
- `npm run build` - typecheck (`tsc -b`) then production build.
- `npm run lint` - oxlint.
- `npm run typecheck` - `tsc -b` only.
- `npm run generate:api` - regenerate `src/api/schema.d.ts` from `../api/openapi.yaml`.
  Run after every contract change; CI fails if the generated file drifts from the spec.

## API access

Always go through `src/api/client.ts` (openapi-fetch typed by the generated schema).
Requests are same-origin so the httpOnly auth cookies from Milestone 1 work without CORS.
