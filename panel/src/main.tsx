import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
// Подмножества под двуязычный UI (EN/RU): латиница + кириллица, sans + mono.
import '@fontsource/ibm-plex-sans/cyrillic-400.css';
import '@fontsource/ibm-plex-sans/cyrillic-500.css';
import '@fontsource/ibm-plex-sans/cyrillic-600.css';
import '@fontsource/ibm-plex-sans/latin-400.css';
import '@fontsource/ibm-plex-sans/latin-500.css';
import '@fontsource/ibm-plex-sans/latin-600.css';
import '@fontsource/ibm-plex-mono/cyrillic-400.css';
import '@fontsource/ibm-plex-mono/latin-400.css';
import '@fontsource/ibm-plex-mono/latin-500.css';
import './styles/app.css';
import App from './App';

const container = document.getElementById('root');
if (container === null) throw new Error('missing #root element');
createRoot(container).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
