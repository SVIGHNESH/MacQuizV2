import { createContext, useContext } from 'react'
import type { components } from '../api/schema'

export type SessionUser = components['schemas']['User']
export type ApiError = components['schemas']['Error']

export type AuthState =
  | { phase: 'loading' }
  | { phase: 'signed-out' }
  | { phase: 'signed-in'; user: SessionUser }

/** Thrown by auth actions so screens can render the API's stable error codes. */
export class AuthActionError extends Error {
  readonly code: string
  readonly fields?: Record<string, string>
  readonly retryAfterSeconds?: number

  constructor(err: ApiError, retryAfterSeconds?: number) {
    super(err.message)
    this.code = err.code
    this.fields = err.fields
    this.retryAfterSeconds = retryAfterSeconds
  }
}

export interface AuthContextValue {
  state: AuthState
  login: (email: string, password: string) => Promise<void>
  logout: () => Promise<void>
  changePassword: (currentPassword: string, newPassword: string) => Promise<void>
}

export const AuthContext = createContext<AuthContextValue | null>(null)

export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext)
  if (!ctx) throw new Error('useAuth must be used inside <AuthProvider>')
  return ctx
}
