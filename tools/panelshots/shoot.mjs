// Съёмка витрины: поднимает панель в демо-режиме (panel/demo.html), снимает
// три экрана в docs/images. Одна команда вместо часа ручной работы — иначе
// следующее обновление README снова застанет скриншоты пустого стенда.
//
// Тема, язык и проект форсятся через addInitScript, то есть ДО первого скрипта
// страницы: иначе сработал бы системный выбор машины оператора и картинки
// разъехались бы по темам между прогонами.
//
// Переходы по разделам — КЛИКОМ, без перезагрузки: роутинг панели живёт на
// pathname, и перезагрузка на /deploys отдала бы настоящий index.html вместо
// демо-входа.
import { spawn } from 'node:child_process';
import { setTimeout as sleep } from 'node:timers/promises';
import { fileURLToPath } from 'node:url';
import path from 'node:path';
import { chromium } from 'playwright';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');
const PORT = 5199;
const OUT = path.join(ROOT, 'docs/images');

/**
 * Что снимаем: файл, пункт нава (null — стартовый экран), маркер готовности и
 * высота кадра. Высота у каждого экрана своя: панель кладёт контент сверху, и
 * единый кадр в 900 px оставил бы Обзору пустую нижнюю треть — на витрине это
 * читается как «тут ничего нет», а не как «экран компактный».
 */
const SHOTS = [
  { file: 'panel-overview.png', nav: null, ready: 'text=Players online', height: 760 },
  { file: 'panel-deploys.png', nav: '/deploys', ready: 'text=1.15.0', height: 740 },
  { file: 'panel-stats.png', nav: '/stats', ready: 'text=Matchmaker queue depth', height: 900 },
];

const dev = spawn('npm', ['run', 'dev:demo', '--', '--port', String(PORT), '--strictPort'], {
  cwd: path.join(ROOT, 'panel'),
  stdio: 'inherit',
});
let browser;
process.on('exit', () => dev.kill());

async function waitForServer() {
  for (let i = 0; i < 120; i++) {
    try {
      const res = await fetch(`http://localhost:${PORT}/demo.html`);
      if (res.ok) return;
    } catch {
      // сервер ещё не поднялся — ждём
    }
    await sleep(500);
  }
  throw new Error('dev-сервер не поднялся за 60 с');
}

try {
  await waitForServer();
  browser = await chromium.launch();
  const ctx = await browser.newContext({
    viewport: { width: 1440, height: 900 },
    // 2x: нынешние картинки сняты в 1x и мылят текст на ретине в GitHub.
    deviceScaleFactor: 2,
    colorScheme: 'dark',
    locale: 'en-US',
  });
  await ctx.addInitScript(() => {
    localStorage.setItem('birdman.theme', 'dark');
    localStorage.setItem('birdman.lang', 'en');
    localStorage.setItem('birdman.project', 'nova-arena');
  });
  const page = await ctx.newPage();
  await page.goto(`http://localhost:${PORT}/demo.html`);
  // Анимации гасим: пульс живого индикатора и переходы графиков дают дрожь
  // между кадрами, из-за которой один и тот же экран снимается по-разному.
  await page.addStyleTag({
    content: '*, *::before, *::after { animation: none !important; transition: none !important; }',
  });

  for (const shot of SHOTS) {
    if (shot.nav !== null) await page.locator(`a[href="${shot.nav}"]`).click();
    await page.setViewportSize({ width: 1440, height: shot.height });
    await page.waitForSelector(shot.ready, { timeout: 20_000 });
    await page.waitForTimeout(1500); // догрузка графиков после первого рендера
    await page.screenshot({ path: path.join(OUT, shot.file) });
    console.log(`снято: ${shot.file}`);
  }
} finally {
  await browser?.close();
  dev.kill();
}
