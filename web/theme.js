// Applies the saved theme before the page paints, so it never flashes the default.
try { document.documentElement.dataset.theme = localStorage.getItem('suspt-theme') || 'night'; } catch { /* storage blocked */ }
