// ESLint (flat config) для панели. Заведён по трекеру #31: дефект #948 —
// переменная использовалась внутри `useMemo`, но отсутствовала в его deps —
// проехал в main, потому что линтера не было вовсе, а `tsc` таких вещей не
// проверяет by design. Правило `react-hooks/exhaustive-deps` поймало бы его
// даром.
//
// ПОЛИТИКА НАБОРА ПРАВИЛ. Здесь курируемый набор, а не «recommended целиком»:
// линтер, который приходится массово глушить, чтобы стало зелено, хуже
// отсутствующего — он создаёт ложное чувство защиты и приучает пролистывать
// вывод. Каждое отклонение от дефолта ниже объяснено; отключений «чтобы
// прошло» здесь нет.
//
// СЕВЕРИТИ И ГЕЙТ. Правила корректности — `error`, правила про DX — `warn`.
// Скрипт `npm run lint` гоняет `eslint . --max-warnings 0`, так что роняет и
// то и другое: варнинг, который ничего не роняет, — это мусор в выводе.
// Разделение остаётся смысловым — оно говорит читателю, что именно сломано.

import js from '@eslint/js';
import globals from 'globals';
import reactHooks from 'eslint-plugin-react-hooks';
import reactRefresh from 'eslint-plugin-react-refresh';
import tseslint from 'typescript-eslint';

export default tseslint.config(
  { ignores: ['dist', 'node_modules', 'coverage'] },
  {
    files: ['**/*.{js,ts,tsx}'],
    extends: [js.configs.recommended, ...tseslint.configs.recommended],
    languageOptions: {
      ecmaVersion: 2022,
      globals: { ...globals.browser, ...globals.es2022 },
    },
    plugins: {
      'react-hooks': reactHooks,
      'react-refresh': reactRefresh,
    },
    rules: {
      // ── React-хуки ────────────────────────────────────────────────────
      'react-hooks/rules-of-hooks': 'error',

      // `error`, а не `warn` (дефолт плагина): это правило — причина, по
      // которой линтер здесь вообще появился. При `--max-warnings 0` разницы
      // в поведении нет, но `error` не даёт будущему автору принять находку
      // за совет.
      //
      // `additionalHooks` — существенное расширение покрытия. Данные панель
      // тянет не голым `useEffect`, а своими хуками вида `(fetcher, deps)`:
      // useData/useLiveAsync (lib/live.tsx), useAsync (lib/useAsync.ts),
      // useProjectList (lib/project.tsx). Их массив deps — такой же ручной
      // список, как у useMemo, и до сих пор он был слепой зоной ЛЮБОГО
      // линтера: забыть в нём проект или фильтр можно было безнаказанно на
      // каждом из ~40 call-site'ов по экранам. Замер на момент заведения:
      // новых находок ноль (экраны честны) — цена нулевая, защита реальная.
      'react-hooks/exhaustive-deps': [
        'error',
        { additionalHooks: '^(useData|useLiveAsync|useAsync|useProjectList)$' },
      ],

      // ── React Fast Refresh (DX) ───────────────────────────────────────
      // Правило про гранулярность HMR в `npm run dev`, не про корректность.
      // Стиль панели — держать чистый хелпер рядом с компонентом, которому он
      // служит, и покрывать его юнит-тестом напрямую (toneOfNodeState в
      // Badge.tsx, keepForEnv в env.tsx, useSession в session.tsx …). Такие
      // файлы помечены `/* eslint-disable react-refresh/only-export-components */`
      // в шапке — поштучно, а не одной строкой здесь: правило остаётся живым,
      // и в НОВОМ файле оно сработает, а автор примет решение осознанно.
      'react-refresh/only-export-components': ['warn', { allowConstantExport: true }],

      // ── Прочее ────────────────────────────────────────────────────────
      // `allowEmptyCase` — не послабление, а ровно тот случай, для которого
      // опция сделана. Панель группирует метки case'ов подряд
      // (`case 'a': case 'b': return 'warn';`) и документирует отдельные метки
      // комментарием над ними. Пустой case с комментарием внутри дефолтный
      // `no-fallthrough` считает провалом. Настоящий проваливающийся case —
      // тот, где есть выполняемые операторы без break, — по-прежнему ошибка.
      'no-fallthrough': ['error', { allowEmptyCase: true }],
    },
  },
);
