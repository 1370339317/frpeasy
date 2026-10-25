/** 后端接口封装。所有请求走这里的单一入口，401 统一交给外层踢回登录页。 */

export class ApiError extends Error {
  readonly status: number
  constructor(message: string, status: number) {
    super(message)
    this.name = 'ApiError'
    this.status = status
  }
}

/** 会话失效（401）。App 捕获后切回登录页。 */
export class UnauthorizedError extends Error {
  constructor() {
    super('未登录')
    this.name = 'UnauthorizedError'
  }
}

export async function api<T>(
  method: 'GET' | 'POST' | 'PUT' | 'DELETE',
  path: string,
  body?: unknown,
): Promise<T> {
  const init: RequestInit = { method, headers: {} }
  if (body !== undefined) {
    init.headers = { 'Content-Type': 'application/json' }
    init.body = JSON.stringify(body)
  }
  const res = await fetch(path, init)
  if (res.status === 401) throw new UnauthorizedError()

  const text = await res.text()
  let data: unknown = {}
  if (text) {
    try {
      data = JSON.parse(text)
    } catch {
      data = { error: text }
    }
  }
  if (!res.ok) {
    const msg =
      typeof data === 'object' && data !== null && 'error' in data
        ? String((data as { error: unknown }).error)
        : `请求失败 ${res.status}`
    throw new ApiError(msg, res.status)
  }
  return data as T
}

/** 拼查询串时统一 encode，避免镜像前缀里的 :// 和 / 被拼坏。 */
export function qs(params: Record<string, string | number | boolean>): string {
  const p = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) p.set(k, String(v))
  return p.toString()
}
