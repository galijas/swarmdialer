document.getElementById('login-form').addEventListener('submit', async (ev) => {
  ev.preventDefault();
  const btn = document.getElementById('login-btn');
  const status = document.getElementById('login-status');
  btn.disabled = true;
  status.textContent = 'Logging in...';
  status.className = 'status-line';
  try {
    const res = await fetch('/api/login', {
      method: 'POST', headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({
        username: document.getElementById('login-username').value.trim(),
        password: document.getElementById('login-password').value,
      }),
    });
    const body = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(body.error || res.statusText);
    location.href = '/';
  } catch (e) {
    status.textContent = e.message;
    status.className = 'status-line error';
    btn.disabled = false;
  }
});
