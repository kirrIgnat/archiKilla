'use strict';
window.miniAppSession = '';
async function startTelegramApp() {
  const inside = new URLSearchParams(location.search).get('telegram') === '1' || location.hash.includes('tgWebAppData=');
  if (!inside) return enter();
  await new Promise((resolve, reject) => {
    const script = document.createElement('script');
    script.src = 'https://telegram.org/js/telegram-web-app.js';
    const timer = setTimeout(() => reject(Error('Не удалось загрузить Telegram. Закройте приложение и откройте снова.')), 15000);
    script.onload = () => { clearTimeout(timer); resolve(); };
    script.onerror = () => { clearTimeout(timer); reject(Error('Не удалось подключиться к Telegram.')); };
    document.head.appendChild(script);
  });
  const tg = window.Telegram?.WebApp;
  if (!tg?.initData) throw Error('Откройте приложение кнопкой в личном чате с ботом.');
  tg.ready(); tg.expand();
  const response = await fetch('/api/telegram/login', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ init_data: tg.initData })
  });
  const data = await response.json();
  if (!response.ok) throw Error(data.error || 'Не удалось войти через Telegram.');
  window.miniAppSession = data.token;
  await enter();
  S.page = admin() ? 'overview' : 'schedule';
  render();
  tg.BackButton?.hide();
}
