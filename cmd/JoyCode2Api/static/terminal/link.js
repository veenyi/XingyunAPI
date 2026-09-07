// /terminal/link.js — 行云API 面板悬浮入口：跳转 Web SSH 终端页。
(function () {
  if (window.__xingyunTerminalLink) return;
  window.__xingyunTerminalLink = true;

  function mount() {
    if (document.getElementById('xy-terminal-fab')) return;
    var btn = document.createElement('div');
    btn.id = 'xy-terminal-fab';
    btn.title = '云主机 Web SSH 终端';
    btn.style.cssText = [
      'position:fixed', 'right:20px', 'bottom:76px', 'z-index:9999',
      'width:44px', 'height:44px', 'border-radius:50%',
      'background:#1677ff', 'color:#fff', 'display:flex',
      'align-items:center', 'justify-content:center',
      'box-shadow:0 4px 12px rgba(0,0,0,.25)', 'cursor:pointer',
      'font-size:20px', 'user-select:none', 'opacity:.85'
    ].join(';');
    btn.innerHTML = '&gt;_';
    btn.onmouseenter = function () { btn.style.opacity = '1'; };
    btn.onmouseleave = function () { btn.style.opacity = '.85'; };
    btn.onclick = function () {
      var t = localStorage.getItem('joycode_jwt') || localStorage.getItem('jwt') || '';
      window.open('/terminal/index.html' + (t ? '' : ''), '_blank');
    };
    document.body.appendChild(btn);
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', mount);
  } else {
    mount();
  }
})();
