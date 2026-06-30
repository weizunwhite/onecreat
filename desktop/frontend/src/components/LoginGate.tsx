import { useState } from "react";
import { app } from "../lib/bridge";
import logo from "../assets/onecreat-logo.png";

// 登录门:未登录时挡住整个 app。功能由管理员分配,登录后客户端按权限显示。
// (P1 mock 账号:admin/admin = 超管全功能;demo/demo = 演示客户部分功能。)

export function LoginGate({ onLoggedIn }: { onLoggedIn: () => void }) {
  const [account, setAccount] = useState("");
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const submit = async () => {
    if (!account.trim() || !password || busy) return;
    setBusy(true);
    setError(null);
    try {
      const r = await app.AccountLogin(account.trim(), password);
      if (r.ok) onLoggedIn();
      else setError(r.error ?? "登录失败");
    } catch (e) {
      setError(String((e as Error)?.message ?? e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="login">
      <div className="login__card">
        <img src={logo} className="login__logo" alt="OneCreat" />
        <div className="login__title">OneCreat</div>
        <div className="login__sub">登录以使用 · 功能由管理员分配</div>
        <input
          className="login__in"
          placeholder="账号"
          value={account}
          autoFocus
          onChange={(e) => setAccount(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") document.getElementById("login-pw")?.focus();
          }}
        />
        <input
          id="login-pw"
          className="login__in"
          type="password"
          placeholder="密码"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") void submit();
          }}
        />
        {error && <div className="login__err">{error}</div>}
        <button className="btn btn--primary login__btn" disabled={busy || !account.trim() || !password} onClick={() => void submit()}>
          {busy ? "登录中…" : "登录"}
        </button>
      </div>
    </div>
  );
}
