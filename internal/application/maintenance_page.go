package application

import (
	"fmt"
	"html"
	"path/filepath"
	"strings"

	"masamong/internal/i18n"
	"masamong/internal/launcher"
)

var maintenanceCatalog = i18n.New()

// AcceptedLocale is the best catalog language for an Accept-Language header ("ko-KR,ko;q=0.9" -> "ko").
func AcceptedLocale(header string) string {
	for _, part := range strings.Split(header, ",") {
		tag := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
		if tag == "" || tag == "*" {
			continue
		}
		if loc := maintenanceCatalog.Resolve(tag); loc != "en" || strings.EqualFold(strings.SplitN(tag, "-", 2)[0], "en") {
			return loc
		}
	}
	return "en"
}

// MaintenancePage is the page the daemon serves while it is stopped for maintenance: plain HTML with no
// script, in the visitor's language. It says in everyday words what happened, that the bot is stopped,
// and exactly which commands can work in this state (MaintenanceCase), each with what it does. The raw
// error is never shown (it can hold paths, URLs or credentials); technical detail is in the log.
func MaintenancePage(dataRoot, locale string) string {
	locale = maintenanceCatalog.Resolve(locale)
	t := func(key string, args ...any) string { return maintenanceCatalog.T(locale, key, args...) }
	kind, commands := MaintenanceCase(dataRoot)
	var b strings.Builder
	fmt.Fprintf(&b, "<!doctype html>\n<html lang=%q>\n<head>\n<meta charset=\"utf-8\">\n<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n", locale)
	fmt.Fprintf(&b, "<title>%s</title>\n<style>%s</style>\n</head>\n<body>\n<main>\n", html.EscapeString(t("maintenance.page.title")), maintenanceCSS)
	fmt.Fprintf(&b, "<h1>%s</h1>\n<p>%s</p>\n", html.EscapeString(t("maintenance.page.title")), html.EscapeString(t("maintenance.page.stopped")))
	fmt.Fprintf(&b, "<h2>%s</h2>\n<p>%s</p>\n", html.EscapeString(t("maintenance.page.what")), html.EscapeString(t("maintenance.case."+kind)))
	fmt.Fprintf(&b, "<h2>%s</h2>\n", html.EscapeString(t("maintenance.page.todo")))
	if len(commands) == 0 {
		fmt.Fprintf(&b, "<p>%s</p>\n", html.EscapeString(t("maintenance.page.none")))
	} else {
		stop := "masamong stop"
		if filepath.Clean(dataRoot) != filepath.Clean(launcher.DefaultDataRoot()) {
			stop = fmt.Sprintf("masamong --data-root \"%s\" stop", dataRoot)
		}
		fmt.Fprintf(&b, "<p>%s</p>\n<pre>%s</pre>\n", html.EscapeString(t("maintenance.page.step_stop")), html.EscapeString(stop))
		step := "maintenance.page.step_run_one"
		if len(commands) > 1 {
			step = "maintenance.page.step_run_many"
		}
		fmt.Fprintf(&b, "<p>%s</p>\n<ul>\n", html.EscapeString(t(step)))
		for _, c := range commands {
			line := fmt.Sprintf("masamongd --data-root \"%s\" --maintenance %s", dataRoot, c)
			fmt.Fprintf(&b, "<li><pre>%s</pre><p>%s</p></li>\n", html.EscapeString(line),
				html.EscapeString(t("maintenance.effect."+c+"."+effectVariant(kind, c))))
		}
		fmt.Fprintf(&b, "</ul>\n<p>%s</p>\n", html.EscapeString(t("maintenance.page.step_start")))
	}
	fmt.Fprintf(&b, "<p class=\"small\">%s: <code>%s</code><br>%s</p>\n", html.EscapeString(t("maintenance.page.folder")), html.EscapeString(dataRoot),
		html.EscapeString(t("maintenance.page.log", "path", filepath.Join(dataRoot, "logs", "daemon.log"))))
	b.WriteString("</main>\n</body>\n</html>\n")
	return b.String()
}

const maintenanceCSS = `:root{color-scheme:light dark}body{margin:0;font:16px/1.6 system-ui,-apple-system,"Segoe UI",sans-serif;background:Canvas;color:CanvasText}` +
	`main{max-width:42rem;margin:0 auto;padding:2rem 1rem;overflow-wrap:anywhere;word-break:keep-all}h1{font-size:1.5rem;margin:0 0 .5rem}h2{font-size:1.1rem;margin:1.5rem 0 .25rem}` +
	`pre{background:color-mix(in srgb,CanvasText 8%,Canvas);padding:.6rem .8rem;border-radius:.4rem;overflow-x:auto;margin:.4rem 0;white-space:pre-wrap;word-break:break-all}` +
	`code{overflow-wrap:anywhere;word-break:break-all}ul{list-style:none;padding:0}li{margin:0 0 1rem}li p{margin:0}.small{font-size:.85rem;opacity:.75}`
