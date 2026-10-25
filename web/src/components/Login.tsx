import { useState } from 'react'
import { api } from '../lib/api'
import { Button, Field, inputCls } from '../components/ui'

export function Login({ onSuccess }: { onSuccess: () => void }) {
  const [user, setUser] = useState('admin')
  const [pass, setPass] = useState('')
  const [msg, setMsg] = useState('')
  const [busy, setBusy] = useState(false)

  async function submit() {
    if (busy) return
    setMsg('')
    setBusy(true)
    try {
      await api('POST', '/api/login', { user, pass })
      setPass('')
      onSuccess()
    } catch (e) {
      setMsg((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="fixed inset-0 z-100 flex items-center justify-center bg-bg">
      <form
        className="w-[360px] rounded-2xl border border-border bg-card px-[26px] py-7 shadow-panel"
        onSubmit={(e) => {
          e.preventDefault()
          void submit()
        }}
      >
        <div className="mb-1 text-xl font-bold tracking-[0.4px]">
          frp<span className="text-accent">easy</span>
        </div>
        <div className="mb-5 text-[12.5px] text-muted">把 FRP 穿透变成几次点击</div>

        <Field label="用户名">
          <input
            className={inputCls}
            value={user}
            autoComplete="username"
            onChange={(e) => setUser(e.target.value)}
          />
        </Field>
        <Field label="密码">
          <input
            className={inputCls}
            type="password"
            value={pass}
            autoComplete="current-password"
            placeholder="初始口令 admin"
            autoFocus
            onChange={(e) => setPass(e.target.value)}
          />
        </Field>

        <Button type="submit" className="w-full" disabled={busy}>
          {busy ? '登录中…' : '登 录'}
        </Button>
        <div className="mt-1 min-h-[18px] text-xs text-bad-fg">{msg}</div>
      </form>
    </div>
  )
}
