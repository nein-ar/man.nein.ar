// the host serves 404.html for every missing path, so the terminal fills
// in the real one at load time
document.addEventListener('DOMContentLoaded', () => {
    document.getElementById('cmd-url').textContent = window.location.pathname;
    document.getElementById('log-url').textContent = window.location.href;

    const now = new Date().toISOString().replace('T', ' ').split('.')[0];
    document.querySelectorAll('.curr-time').forEach(el => { el.textContent = now; });
});
