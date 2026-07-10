import { AuthProvider } from './auth/auth'
import { useAuth } from './auth/context'
import { ToastProvider } from './toast/Toast'
import LoginScreen from './screens/LoginScreen'
import ChangePasswordScreen from './screens/ChangePasswordScreen'
import AuthoringWorkspace from './authoring/AuthoringWorkspace'
import StudentWorkspace from './player/StudentWorkspace'
import AdminWorkspace from './admin/AdminWorkspace'
import './App.css'

function Screens() {
  const { state } = useAuth()

  if (state.phase === 'loading') {
    return (
      <main className="shell">
        <p className="boot-note" role="status">
          Loading…
        </p>
      </main>
    )
  }

  if (state.phase === 'signed-out') {
    return <LoginScreen />
  }

  if (state.user.must_change_password) {
    return <ChangePasswordScreen user={state.user} />
  }

  if (state.user.role === 'teacher') {
    return <AuthoringWorkspace user={state.user} />
  }

  if (state.user.role === 'student') {
    return <StudentWorkspace user={state.user} />
  }

  return <AdminWorkspace user={state.user} />
}

export default function App() {
  return (
    <ToastProvider>
      <AuthProvider>
        <Screens />
        {/* The SDC credit floats over every screen, the quiz player
            included. pointer-events: none - it is a watermark, so it must
            never intercept a click meant for the page under it. */}
        <img
          className="sdc-float"
          src="/sdc-logo.png"
          alt="Software Development Cell"
        />
      </AuthProvider>
    </ToastProvider>
  )
}
