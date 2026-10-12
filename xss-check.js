const { chromium } = require('playwright-core');
const TOKEN = 'lar1_XtAsuddemDcMxqwfpIVSKwmYFWwU24rC';
const EVIL = [
  '<script>alert(1)<\/script>',
  '<img src=x onerror=alert(2)>',
  '[click](javascript:alert(3))',
  '[safe](https://example.com)',
  '<a href="data:text/html,<script>alert(4)<\/script>">d</a>',
  '`<svg onload=alert(5)>`',
].join('\n\n');
(async () => {
  const b = await chromium.launch({ executablePath: '/home/zb2/.cache/ms-playwright/chromium-1234/chrome-linux64/chrome', args:['--no-sandbox'] });
  const pg = await b.newPage({ viewport: { width: 1280, height: 900 } });
  let dialogs = 0;
  pg.on('dialog', async d => { dialogs++; await d.dismiss(); });
  await pg.route('**/v1/**', (route) => {
    const path = route.request().url();
    if (path.includes('/custos/cards/events')) return route.fulfill({ status: 200, headers: {'Content-Type': 'text/event-stream'}, body: ': ka\n\n' });
    if (path.includes('/events?')) return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ in_flight: false, events: [ { t: 'msg', role: 'assistant', text: EVIL } ] }) });
    if (path.includes('/sessions') && route.request().method() === 'GET') return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify([{ id: 'x1', title: 'xss probe' }]) });
    if (path.includes('/sessions')) return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ id: 'x1' }) });
    return route.fallback();
  });
  await pg.goto('http://127.0.0.1:7717/#' + TOKEN);
  await pg.waitForTimeout(1500);
  const audit = await pg.evaluate(() => {
    const t = document.getElementById('transcript');
    return {
      scriptTags: t.querySelectorAll('script').length,
      imgTags: t.querySelectorAll('img').length,
      svgTags: t.querySelectorAll('svg').length,
      anchors: Array.from(t.querySelectorAll('a')).map(a => a.getAttribute('href')),
      onerrorAttrs: t.innerHTML.includes('onerror') && t.querySelector('[onerror]') !== null,
      textShown: t.textContent.includes('alert(1)') && t.textContent.includes('javascript:alert(3)'),
    };
  });
  console.log(JSON.stringify(audit, null, 1));
  console.log('dialogs fired:', dialogs);
  await b.close();
})();
