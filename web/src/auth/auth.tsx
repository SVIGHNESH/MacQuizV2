import { useCallback, useEffect, useMemo, useState, type ReactNode } from 'react'
import { api } from '../api/client'
import {
  AuthActionError,
  AuthContext,
  type ApiError,
  type AuthState,
  type SessionUser,
} from './context'

const NETWORK_ERROR: ApiError = {
  code: 'NETWORK',
  message: 'Could not reach the server. Check your connection and try again.',
}

function retryAfter(response: Response): number | undefined {
  const raw = response.headers.get('Retry-After')
  if (!raw) return undefined
  const seconds = Number.parseInt(raw, 10)
  return Number.isNaN(seconds) ? undefined : seconds
}

export function AuthProvider({ children }: { children: ReactNode }) {
  const [state, setState] = useState<AuthState>({ phase: 'loading' })

  // Session bootstrap: the access cookie lives 15 minutes, so a returning
  // visitor usually needs one silent refresh before /me answers.
  useEffect(() => {
    let cancelled = false
    const commit = (next: AuthState) => {
      if (!cancelled) setState(next)
    }
    ;(async () => {
      try {
        const me = await api.GET('/api/v1/auth/me')
        if (me.data) return commit({ phase: 'signed-in', user: me.data.user })
        const refreshed = await api.POST('/api/v1/auth/refresh')
        if (refreshed.data) {
          return commit({ phase: 'signed-in', user: refreshed.data.user })
        }
        commit({ phase: 'signed-out' })
      } catch {
        commit({ phase: 'signed-out' })
      }
    })()
    return () => {
      cancelled = true
    }
  }, [])

  const login = useCallback(async (email: string, password: string) => {
    const result = await api
      .POST('/api/v1/auth/login', { body: { email, password } })
      .catch(() => null)
    if (!result) throw new AuthActionError(NETWORK_ERROR)
    if (result.data) {
      setState({ phase: 'signed-in', user: result.data.user })
      return
    }
    throw new AuthActionError(result.error, retryAfter(result.response))
  }, [])

  const logout = useCallback(async () => {
    try {
      await api.POST('/api/v1/auth/logout')
    } catch {
      // Cookies may survive a network failure, but locally we are signed out
      // either way; the server rejects the stale session on next use.
    }
    setState({ phase: 'signed-out' })
  }, [])

  const changePassword = useCallback(
    async (currentPassword: string, newPassword: string) => {
      if (state.phase !== 'signed-in') return
      const result = await api
        .POST('/api/v1/auth/password', {
          body: { current_password: currentPassword, new_password: newPassword },
        })
        .catch(() => null)
      if (!result) throw new AuthActionError(NETWORK_ERROR)
      if (result.error) {
        throw new AuthActionError(result.error, retryAfter(result.response))
      }
      // A password change revokes every session and clears the cookies, so
      // sign straight back in with the new credential to keep the user moving
      // (this is the forced first-login path for provisioned accounts).
      try {
        const relogin = await api.POST('/api/v1/auth/login', {
          body: { email: state.user.email, password: newPassword },
        })
        if (relogin.data) {
          setState({ phase: 'signed-in', user: relogin.data.user })
          return
        }
      } catch {
        // Fall through: the change itself succeeded.
      }
      setState({ phase: 'signed-out' })
    },
    [state],
  )

  const updateUser = useCallback((user: SessionUser) => {
    setState((prev) => (prev.phase === 'signed-in' ? { phase: 'signed-in', user } : prev))
  }, [])

  const value = useMemo(
    () => ({ state, login, logout, changePassword, updateUser }),
    [state, login, logout, changePassword, updateUser],
  )

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>
}
