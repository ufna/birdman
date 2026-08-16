// Вход демо-режима: ставим fetch-мок, приводим путь к корню (роутинг панели —
// по pathname, а стартуем мы с /demo.html), дальше — ОБЫЧНЫЙ бутстрап панели.
// Динамический импорт, а не статический: подмена fetch обязана произойти до
// первого запроса приложения.
import { installDemoFetch } from './router';

installDemoFetch();
window.history.replaceState(null, '', '/');
void import('../main');
