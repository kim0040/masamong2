//go:build !noweb

package web

import (
	"context"
	"encoding/json"
	"html/template"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"

	"masamong/internal/action"
	"masamong/internal/launcher"
)

// /lite is the emergency page: plain HTML, no JavaScript, for when the app cannot load (old browser,
// broken build, bad connection). It shows the instance status and offers pause/resume and restart.

var liteTpl = template.Must(template.New("lite").Parse(`<!doctype html>
<html lang="{{.Lang}}"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.T.title}}</title><link rel="icon" href="/brand/favicon.svg">
<style>
:root{color-scheme:light dark;--bg:#FAFAF9;--fg:#1C1917;--muted:#6B6560;--line:#E7E5E4;--accent:#C2410C}
@media (prefers-color-scheme:dark){:root{--bg:#0E0E10;--fg:#F4F4F5;--muted:#A1A1AA;--line:#2A2A2F;--accent:#FF9F5A}}
body{margin:0;font:16px/1.5 system-ui,-apple-system,"Segoe UI","Apple SD Gothic Neo","Malgun Gothic",sans-serif;background:var(--bg);color:var(--fg)}
:lang(ja) body{font-family:system-ui,-apple-system,"Hiragino Sans","Yu Gothic UI","Meiryo UI",sans-serif}
:lang(zh) body{font-family:system-ui,-apple-system,"PingFang SC","Microsoft YaHei UI",sans-serif}
code{font:.9em ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;overflow-wrap:anywhere}
main{max-width:40rem;margin:0 auto;padding:2rem 1rem}
h1{font-size:1.5rem;line-height:1.2;display:flex;gap:.75rem;align-items:center}
dl{display:grid;grid-template-columns:max-content 1fr;gap:.5rem 1rem}dt{color:var(--muted)}
form{display:inline-block;margin:.5rem .5rem 0 0}
button,input{font:inherit;padding:.6rem 1rem;border-radius:.5rem;border:1px solid var(--line);background:transparent;color:inherit;min-height:44px}
button.primary{background:var(--accent);color:#fff;border-color:var(--accent)}
p.note{color:var(--muted)}a{color:var(--accent)}
</style></head><body><main>
<h1><img src="/brand/logo-mark.svg" width="40" height="40" alt="">{{.T.title}}</h1>
{{if .Authed}}
<dl><dt>{{.T.instance}}</dt><dd>{{.Instance}}</dd><dt>{{.T.version}}</dt><dd>{{.Version}}</dd>
<dt>{{.T.state}}</dt><dd>{{if .Paused}}{{.T.paused}}{{else}}{{.T.running}}{{end}}</dd></dl>
<form method="post" action="/lite"><input type="hidden" name="csrf" value="{{.CSRF}}">
{{if .Paused}}<button class="primary" name="do" value="resume">{{.T.resume}}</button>{{else}}<button name="do" value="pause">{{.T.pause}}</button>{{end}}</form>
<form method="post" action="/lite"><input type="hidden" name="csrf" value="{{.CSRF}}"><button name="do" value="restart">{{.T.restart}}</button></form>
{{if .Msg}}<p role="status">{{.Msg}}</p>{{end}}
<p class="note"><a href="/">{{.T.full}}</a></p>
{{else}}
<p class="note">{{.T.signin_note}}</p>
<p class="note">{{.T.signin_cmd}}<br><code>{{.TokenCmd}}</code>{{if .TokenCmdCmd}}<br>{{.T.signin_cmd_cmd}} <code>{{.TokenCmdCmd}}</code>{{end}}</p>
<p class="note">{{.T.signin_file}} <code>{{.TokenFile}}</code></p>
{{if .Msg}}<p role="alert">{{.Msg}}</p>{{end}}
<form method="post" action="/lite"><label>{{.T.token}} <input name="token" type="password" autocomplete="current-password" required></label>
<button class="primary">{{.T.signin}}</button></form>
{{end}}
</main></body></html>`))

// The emergency page speaks every language the console does; anything else gets English.
var liteText = map[string]map[string]string{
	"ko": {"title": "마사몽 비상 관리", "instance": "인스턴스", "version": "버전", "state": "상태", "paused": "응답 일시 중지됨", "running": "동작 중",
		"pause": "모든 응답 일시 중지", "resume": "응답 다시 켜기", "restart": "서비스 다시 시작", "full": "전체 관리 화면 열기",
		"signin_note": "관리 토큰으로 로그인하세요. 마사몽이 실행 중인 컴퓨터의 터미널에서 아래 명령으로 토큰을 볼 수 있습니다.",
		"signin_cmd":  "토큰 보기:", "signin_cmd_cmd": "명령 프롬프트(cmd)에서는:", "signin_file": "토큰 파일 위치 (데이터 폴더 안):",
		"token": "관리 토큰", "signin": "로그인", "wrong": "토큰이 맞지 않습니다.", "rate": "시도가 너무 많습니다. 1분 뒤에 다시 하세요.", "done": "처리했습니다."},
	"en": {"title": "Masamong emergency console", "instance": "Instance", "version": "Version", "state": "State", "paused": "Replies paused", "running": "Running",
		"pause": "Pause all replies", "resume": "Resume replies", "restart": "Restart service", "full": "Open the full console",
		"signin_note": "Sign in with the admin token. In a terminal on the computer where Masamong runs, this command prints it.",
		"signin_cmd":  "Show the token:", "signin_cmd_cmd": "In Command Prompt (cmd):", "signin_file": "The token file, inside the data folder:",
		"token": "Admin token", "signin": "Sign in", "wrong": "The token does not match.", "rate": "Too many attempts. Wait a minute.", "done": "Done."},
	"ja": {"title": "マサモン緊急管理", "instance": "インスタンス", "version": "バージョン", "state": "状態", "paused": "返信を一時停止中", "running": "動作中",
		"pause": "すべての返信を一時停止", "resume": "返信を再開", "restart": "サービスを再起動", "full": "通常の管理画面を開く",
		"signin_note": "管理トークンでサインインしてください。Masamong が動いているコンピューターのターミナルで、次のコマンドを実行するとトークンが表示されます。",
		"signin_cmd":  "トークンを表示:", "signin_cmd_cmd": "コマンドプロンプト（cmd）の場合:", "signin_file": "トークンファイルの場所（データフォルダー内）:",
		"token": "管理トークン", "signin": "サインイン", "wrong": "トークンが一致しません。", "rate": "試行回数が多すぎます。1分待ってください。", "done": "完了しました。"},
	"zh-Hans": {"title": "Masamong 应急管理", "instance": "实例", "version": "版本", "state": "状态", "paused": "已暂停回复", "running": "运行中",
		"pause": "暂停所有回复", "resume": "恢复回复", "restart": "重启服务", "full": "打开完整管理界面",
		"signin_note": "请使用管理令牌登录。在运行 Masamong 的电脑上打开终端，运行下面的命令即可显示令牌。",
		"signin_cmd":  "显示令牌：", "signin_cmd_cmd": "在命令提示符（cmd）中：", "signin_file": "令牌文件位置（数据文件夹内）：",
		"token": "管理令牌", "signin": "登录", "wrong": "令牌不匹配。", "rate": "尝试次数过多，请等待一分钟。", "done": "已完成。"},
	"es": {"title": "Consola de emergencia de Masamong", "instance": "Instancia", "version": "Versión", "state": "Estado", "paused": "Respuestas en pausa", "running": "En marcha",
		"pause": "Pausar todas las respuestas", "resume": "Reanudar las respuestas", "restart": "Reiniciar el servicio", "full": "Abrir la consola completa",
		"signin_note": "Inicia sesión con el token de administrador. En un terminal del equipo donde se ejecuta Masamong, este comando lo muestra.",
		"signin_cmd":  "Mostrar el token:", "signin_cmd_cmd": "En el símbolo del sistema (cmd):", "signin_file": "El archivo del token, dentro de la carpeta de datos:",
		"token": "Token de administrador", "signin": "Iniciar sesión", "wrong": "El token no coincide.", "rate": "Demasiados intentos. Espera un minuto.", "done": "Hecho."},
}

// tokenFile names the token file the way the sign-in page can say it without printing the account's
// folder: the platform's default data folder spelled with its own variable, or just the file's place
// inside the data folder when this instance keeps its data elsewhere.
func (s *Server) tokenFile() string {
	if !samePath(s.svc.DataRoot, launcher.OSDataRoot()) {
		return "admin" + string(filepath.Separator) + "web-token"
	}
	switch runtime.GOOS {
	case "windows":
		return `%APPDATA%\Masamong\admin\web-token`
	case "darwin":
		return "~/Library/Application Support/Masamong/admin/web-token"
	}
	return "~/.local/share/masamong/admin/web-token"
}

// liteLang is the console language the browser asked for, when the emergency page has it.
func (s *Server) liteLang(r *http.Request) string {
	if l := s.uiLang(r); liteText[l] != nil {
		return l
	}
	return "en"
}

func (s *Server) liteRender(w http.ResponseWriter, r *http.Request, msg string) {
	s.liteRenderStatus(w, r, http.StatusOK, msg)
}

func (s *Server) liteRenderStatus(w http.ResponseWriter, r *http.Request, status int, msg string) {
	lang := s.liteLang(r)
	_, sess, ok := s.currentSession(r)
	data := map[string]any{"Lang": lang, "T": liteText[lang], "Authed": ok, "Msg": msg}
	if ok {
		data["CSRF"] = sess.CSRF
		data["Instance"] = s.svc.Doc().Instance.ID
		data["Version"] = appVersion()
		data["Paused"] = s.svc.Store != nil && s.svc.Paused("", "")
	} else {
		data["TokenCmd"], data["TokenCmdCmd"], data["TokenFile"] = s.signin["token"], s.signin["token_cmd"], s.tokenFile()
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = liteTpl.Execute(w, data)
}

func (s *Server) litePage(w http.ResponseWriter, r *http.Request) { s.liteRender(w, r, "") }

func (s *Server) liteSubmit(w http.ResponseWriter, r *http.Request) {
	_, sess, ok := s.currentSession(r)
	if !ok && r.ParseForm() == nil && r.PostFormValue("token") != "" {
		s.liteSignIn(w, r)
		return
	}
	if !ok || r.ParseForm() != nil || !s.csrfOK(sess, r.PostFormValue("csrf")) {
		http.Error(w, "sign in required", http.StatusForbidden)
		return
	}
	var req action.Request
	switch r.PostFormValue("do") {
	case "pause", "resume":
		raw, _ := json.Marshal(map[string]any{"scope": "all", "paused": map[bool]string{true: "true", false: "false"}[r.PostFormValue("do") == "pause"]})
		req = action.Request{Action: "runtime.pause", Input: raw}
	case "restart":
		req = action.Request{Action: "runtime.restart"}
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	req.RequestID, req.Locale = "web", s.lang(r)
	res := s.svc.Execute(context.WithoutCancel(r.Context()), req)
	msg := liteText[s.liteLang(r)]["done"]
	if !res.OK && res.Error != nil {
		msg = res.Error.Message
	}
	s.liteRender(w, r, msg)
}

// liteSignIn takes the admin token from the form (a POST, so it never lands in the address bar, the
// browser history or an access log) and signs the browser in.
func (s *Server) liteSignIn(w http.ResponseWriter, r *http.Request) {
	text := liteText[s.liteLang(r)]
	logins := s.loginsFor(r)
	if !logins.allow(s.now()) {
		s.liteRenderStatus(w, r, http.StatusTooManyRequests, text["rate"])
		return
	}
	if !s.tokenMatches(strings.TrimSpace(r.PostFormValue("token"))) {
		logins.fail(s.now())
		s.liteRenderStatus(w, r, http.StatusUnauthorized, text["wrong"])
		return
	}
	id, _ := s.sessions.create(s.now(), r.UserAgent())
	s.setSessionCookie(w, r, id, int(sessionTTL.Seconds()))
	http.Redirect(w, r, "/lite", http.StatusSeeOther)
}
