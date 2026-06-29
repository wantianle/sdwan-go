// ---- Elements ----
const statusDot     = document.getElementById('status-dot');
const statusLabel   = document.getElementById('status-label');
const statusLatency = document.getElementById('status-latency');
const toggleInput   = document.getElementById('toggle-connection');
const serverListEl  = document.getElementById('server-list');
const closeButton   = document.getElementById('btn-close');

// ---- State ----
let currentServerId = '';
let pollTimer = null;

// ---- Init ----
document.addEventListener('DOMContentLoaded', () => {
  window.runtime.EventsOn('panel:shown', () => {
    startPolling();
  });

  window.runtime.EventsOn('panel:hidden', () => {
    stopPolling();
  });

  window.runtime.EventsOn('panel:state-changed', () => {
    refreshStatus();
    refreshServers();
  });

  // Toggle connection (Wails returns Promises — must use .then)
  toggleInput.addEventListener('change', () => {
    window.go.main.App.ToggleConnection().then(connected => {
      toggleInput.checked = connected;
      updateConnectionUI(connected);
      window.go.main.App.GetStatus().then(s => updateStatusUI(s));
    });
  });

  // Edit config
  document.getElementById('btn-edit').addEventListener('click', () => {
    window.go.main.App.EditConfig();
  });

  // Reload
  document.getElementById('btn-reload').addEventListener('click', () => {
    window.go.main.App.Reload().then(() => {
      refreshStatus();
      refreshServers();
    });
  });

  closeButton.addEventListener('click', hidePanel);
});

// ---- API helpers ----

function refreshStatus() {
  window.go.main.App.GetStatus().then(s => updateStatusUI(s));
}

function refreshServers() {
  window.go.main.App.GetServers().then(servers => {
    serverListEl.innerHTML = '';
    servers.forEach(s => {
      if (s.selected === 'true') currentServerId = s.id;
      renderServerItem(s, s.latency || '--');
    });
  });
}

function startPolling() {
  refreshStatus();
  refreshServers();
  stopPolling();
  pollTimer = setInterval(() => {
    refreshStatus();
    refreshServers();
  }, 5000);
}

function stopPolling() {
  if (pollTimer !== null) {
    clearInterval(pollTimer);
    pollTimer = null;
  }
}

function hidePanel() {
  stopPolling();
  window.go.main.App.HidePanel();
}

// ---- UI updaters ----

function updateStatusUI(s) {
  updateConnectionUI(s.connected);
  statusLatency.textContent = s.latency_text || '--';
}

function updateConnectionUI(connected) {
  if (connected) {
    statusDot.classList.add('connected');
    statusLabel.textContent = '已连接';
    toggleInput.checked = true;
  } else {
    statusDot.classList.remove('connected');
    statusLabel.textContent = '已断开';
    toggleInput.checked = false;
  }
}

function renderServerItem(s, latText) {
  const item = document.createElement('div');
  item.className = 'server-item' + (s.selected === 'true' ? ' selected' : '');
  item.dataset.serverId = s.id;

  const dot = document.createElement('div');
  dot.className = 'dot';
  item.appendChild(dot);

  const info = document.createElement('div');
  info.className = 'server-info';

  const name = document.createElement('div');
  name.className = 'name';
  name.textContent = s.name;
  info.appendChild(name);

  const lat = document.createElement('div');
  lat.className = 'latency';
  lat.textContent = latText || '';
  info.appendChild(lat);

  item.appendChild(info);

  item.addEventListener('click', () => {
    window.go.main.App.SelectServer(s.id).then(ok => {
      if (ok) {
        refreshStatus();
        refreshServers();
        setTimeout(() => {
          refreshStatus();
          refreshServers();
        }, 800);
      }
    });
  });

  serverListEl.appendChild(item);
}
