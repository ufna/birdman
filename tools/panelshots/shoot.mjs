// Съёмка витрины: поднимает панель в демо-режиме (panel/demo.html), снимает
// полные экраны и превью в docs/images. Одна команда вместо часа ручной работы — иначе
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
const previewsOnly = process.argv.includes('--previews-only');

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

// Превью снимаются с настоящей адаптивной раскладки панели: на телефоне
// подписи читаются без уменьшения целого десктопа вместе с боковым меню.
const PREVIEWS = [
  { file: 'panel-deploys-preview.png', nav: '/deploys', width: 392, kind: 'deploys' },
  { file: 'panel-stats-preview.png', nav: '/stats', width: 392, kind: 'stats', last: 'Matches running' },
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

async function capturePreview(page, shot) {
  // Графики должны впервые измерить контейнер при целевой ширине, иначе
  // canvas после уменьшения окна может удержать прежнюю ширину сетки.
  await page.setViewportSize({ width: shot.width, height: 1100 });
  await page.goto(`http://localhost:${PORT}/demo.html`);
  await page.addStyleTag({
    content: '*, *::before, *::after { animation: none !important; transition: none !important; }',
  });
  if (shot.width < 768) await page.getByRole('button', { name: 'Menu', exact: true }).click();
  await page.locator(`a[href="${shot.nav}"]:visible`).click();
  await page.evaluate(() => window.scrollTo(0, 0));
  await page.getByText(shot.kind === 'deploys' ? 'Multi-version window' : shot.last, { exact: true }).waitFor();
  await page.waitForTimeout(1500);
  await page.evaluate(() => document.fonts.ready);

  let first;
  let last;
  if (shot.kind === 'deploys') {
    const card = page.locator('section').filter({ has: page.getByRole('heading', { name: 'Deploys', exact: true }) });
    first = card;
    // Широкую таблицу версий оставляем в полном кадре по ссылке.
    last = card.locator(':scope > div[class*="bg-warn-bg"]');
  } else {
    first = page.getByText('Players online', { exact: true }).locator('../..');
    last = page.getByText(shot.last, { exact: true }).locator('../..');
  }
  const top = await first.boundingBox();
  const bottom = await last.boundingBox();
  if (top === null || bottom === null) throw new Error(`Preview content missing: ${shot.file}`);
  const x = Math.floor(Math.min(top.x, bottom.x));
  const y = Math.floor(top.y);
  const width = Math.ceil(Math.max(top.x + top.width, bottom.x + bottom.width)) - x;
  if (x < 0 || x + width > shot.width) throw new Error(`Preview overflows the viewport: ${shot.file}`);
  await page.screenshot({
    path: path.join(OUT, shot.file),
    clip: {
      x, y,
      width,
      height: Math.ceil(bottom.y + bottom.height) - y,
    },
  });
  console.log(`снято: ${shot.file}`);
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

  for (const shot of previewsOnly ? [] : SHOTS) {
    if (shot.nav !== null) await page.locator(`a[href="${shot.nav}"]`).click();
    await page.setViewportSize({ width: 1440, height: shot.height });
    await page.waitForSelector(shot.ready, { timeout: 20_000 });
    await page.waitForTimeout(1500); // догрузка графиков после первого рендера
    await page.screenshot({ path: path.join(OUT, shot.file) });
    console.log(`снято: ${shot.file}`);
  }
  for (const shot of PREVIEWS) await capturePreview(page, shot);
} finally {
  await browser?.close();
  dev.kill();
}
