import { AuthProvider } from './auth/auth'
import { useAuth } from './auth/context'
import LoginScreen from './screens/LoginScreen'
import ChangePasswordScreen from './screens/ChangePasswordScreen'
import HomeScreen from './screens/HomeScreen'
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

  return <HomeScreen user={state.user} />
}

export default function App() {
  return (
    <AuthProvider>
      <Screens />
    </AuthProvider>
  )
}
