// Runs before first paint: apply the saved theme, density, motion and language so the page never
// flashes the wrong theme. The server keeps the real preferences; this is only a local cache.
(function () {
  var p = {}
  try { p = JSON.parse(localStorage.getItem('masamong.ui') || '{}') } catch (e) {}
  var d = document.documentElement
  if (p.theme === 'light' || p.theme === 'dark') d.setAttribute('data-theme', p.theme)
  d.setAttribute('data-density', p.density === 'compact' || p.density === 'large' ? p.density : 'normal')
  if (p.motion === 'reduced' || p.motion === 'full') d.setAttribute('data-motion', p.motion)
  if (p.lang) d.setAttribute('lang', p.lang)
  else if ((navigator.language || '').slice(0, 2) !== 'ko') d.setAttribute('lang', 'en')
})()
