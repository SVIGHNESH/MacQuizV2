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
      </AuthProvider>
    </ToastProvider>
  )
}
