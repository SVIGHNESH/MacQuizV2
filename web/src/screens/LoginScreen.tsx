import { useEffect, useState, type FormEvent } from 'react'
import { AuthActionError, useAuth } from '../auth/context'

function loginErrorMessage(err: AuthActionError): string {
  switch (err.code) {
    case 'INVALID_CREDENTIALS':
      return 'That email and password combination is not right. Check both and try again.'
    case 'RATE_LIMITED':
      return 'Too many attempts. Wait a minute, then try again.'
    case 'VALIDATION_FAILED':
      return 'Enter your email address and password.'
    default:
      return err.message
  }
}

function secondsUntil(deadline: number): number {
  return Math.max(0, Math.ceil((deadline - Date.now()) / 1000))
}

/**
 * Seconds left until an absolute deadline, or 0 when there is none.
 *
 * The value is read off the wall clock during render and the interval only
 * forces the re-render. Two things fall out of that: the very first render
 * after a deadline arrives already shows the full wait (a seconds-in-state
 * version reads 0 until its effect runs, and anything watching for zero would
 * fire on that phantom frame), and a browser that throttles timers in a
 * backgrounded tab makes the countdown coarse rather than wrong. Ticking
 * faster than 1s keeps the shown second from lagging the real one.
 */
function useSecondsUntil(deadline: number | null): number {
  const [, setTick] = useState(0)

  useEffect(() => {
    if (deadline === null) return
    const id = setInterval(() => setTick((t) => t + 1), 250)
    return () => clearInterval(id)
  }, [deadline])

  return deadline === null ? 0 : secondsUntil(deadline)
}

function formatCountdown(seconds: number): string {
  const mm = Math.floor(seconds / 60)
  const ss = seconds % 60
  return `${String(mm).padStart(2, '0')}:${String(ss).padStart(2, '0')}`
}

/**
 * The rate-limited state (docs/11 Sh3): the wait is shown as a live countdown
 * and the only action is disabled until it runs out, so a locked-out student
 * can see the lock end instead of guessing when to press the button again.
 */
function RateLimitNotice({
  secondsLeft,
  announcedSeconds,
}: {
  secondsLeft: number
  announcedSeconds: number
}) {
  return (
    <div className="rate-limit-notice" role="alert">
      <span className="chip chip-lifecycle chip-lifecycle-warning">
        Rate limited
      </span>
      <p className="rate-limit-message">
        Too many sign-in attempts. This protects accounts from password
        guessing.
      </p>
      {/* Everything inside role=alert is re-announced when it changes, so the
          ticking numeral is hidden from the accessibility tree and the spoken
          sentence quotes the wait as it stood when the lockout began. It goes
          stale by design: one announcement of a wait that is nearly right beats
          a fresh interruption every second. */}
      <p className="rate-limit-countdown" aria-hidden="true">
        <span className="rate-limit-countdown-label">Retry in</span>
        <span className="rate-limit-countdown-value tabular">
          {formatCountdown(secondsLeft)}
        </span>
      </p>
      <span className="visually-hidden">
        {`Try again in ${announcedSeconds} second${announcedSeconds === 1 ? '' : 's'}.`}
      </span>
    </div>
  )
}

export default function LoginScreen() {
  const { login } = useAuth()
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [submitting, setSubmitting] = useState(false)
  // `granted` is the wait the server handed us; it is kept alongside the
  // deadline so the spoken announcement can quote a number that never moves.
  const [lockout, setLockout] = useState<{
    until: number
    granted: number
  } | null>(null)

  const secondsLeft = useSecondsUntil(lockout?.until ?? null)
  const locked = lockout !== null && secondsLeft > 0

  useEffect(() => {
    if (lockout !== null && secondsLeft === 0) setLockout(null)
  }, [lockout, secondsLeft])

  async function onSubmit(event: FormEvent) {
    event.preventDefault()
    if (submitting || locked) return
    setSubmitting(true)
    setError(null)
    try {
      await login(email, password)
    } catch (err) {
      // A 429 that carries Retry-After becomes a real deadline. Without the
      // header there is nothing to count down to, so it stays a plain message.
      if (
        err instanceof AuthActionError &&
        err.code === 'RATE_LIMITED' &&
        err.retryAfterSeconds
      ) {
        setLockout({
          until: Date.now() + err.retryAfterSeconds * 1000,
          granted: err.retryAfterSeconds,
        })
      } else {
        setError(
          err instanceof AuthActionError
            ? loginErrorMessage(err)
            : 'Something went wrong. Try again.',
        )
      }
      setSubmitting(false)
    }
  }

  return (
    <main className="shell">
      <section className="card auth-card">
        <span className="brand-mark brand-mark-auth" aria-hidden="true">
          M
        </span>
        <header className="auth-heading">
          <h1 className="page-title">Sign in to MacQuiz</h1>
          <p className="auth-subtitle">
            Use the credentials your administrator issued. Accounts aren't
            self-created.
          </p>
        </header>

        <form className="form" onSubmit={onSubmit} noValidate>
          <div className="field">
            <label className="field-label" htmlFor="login-email">
              Email
            </label>
            <input
              id="login-email"
              className="input"
              type="email"
              autoComplete="username"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              autoFocus
              required
            />
          </div>

          <div className="field">
            <label className="field-label" htmlFor="login-password">
              Password
            </label>
            <input
              id="login-password"
              className="input"
              type="password"
              autoComplete="current-password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              required
            />
          </div>

          {locked ? (
            <RateLimitNotice
              secondsLeft={secondsLeft}
              announcedSeconds={lockout.granted}
            />
          ) : (
            error && (
              <p className="form-error" role="alert">
                {error}
              </p>
            )
          )}

          <button
            className="button button-primary auth-submit"
            type="submit"
            disabled={submitting || locked}
          >
            {submitting ? 'Signing in…' : 'Sign in'}
          </button>

          {/* No self-serve reset exists (docs/08): the only recovery path is
              an administrator reissuing a one-time credential. */}
          <p className="form-footnote">
            First time here? Sign in with the one-time password your
            administrator gave you; you will pick your own right after. Locked
            out? Ask your administrator.
          </p>
        </form>
      </section>
    </main>
  )
}
