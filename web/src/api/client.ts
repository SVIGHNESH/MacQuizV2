import createClient from 'openapi-fetch'
import type { paths } from './schema'

// Typed client over api/openapi.yaml. Same-origin: the Vite dev proxy
// forwards to the Go API in dev, Caddy does in production, so httpOnly
// auth cookies (Milestone 1) work without CORS.
export const api = createClient<paths>({ baseUrl: '/' })
