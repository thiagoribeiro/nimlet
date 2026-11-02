/* Nimlet landing: progressive enhancement only. No dependencies or network requests. */
(() => {
  'use strict';

  const menuToggle = document.getElementById('menu-toggle');
  const mobileNavigation = document.getElementById('mobile-navigation');
  const announcer = document.getElementById('announcer');
  const announce = (message) => { if (announcer) announcer.textContent = message; };

  function setMenuOpen(open) {
    menuToggle.setAttribute('aria-expanded', String(open));
    menuToggle.setAttribute('aria-label', open ? 'Close navigation menu' : 'Open navigation menu');
    mobileNavigation.hidden = !open;
  }

  if (menuToggle && mobileNavigation) {
    menuToggle.addEventListener('click', () => {
      setMenuOpen(menuToggle.getAttribute('aria-expanded') !== 'true');
    });
    mobileNavigation.querySelectorAll('a').forEach((link) => link.addEventListener('click', () => setMenuOpen(false)));
    document.addEventListener('keydown', (event) => {
      if (event.key === 'Escape' && menuToggle.getAttribute('aria-expanded') === 'true') {
        setMenuOpen(false);
        menuToggle.focus();
      }
    });
    window.addEventListener('resize', () => {
      if (window.innerWidth > 960) setMenuOpen(false);
    });
  }

  // HTTP code sample tabs. The snippets in the DOM are also the copied source.
  const tabs = [...document.querySelectorAll('[data-code-tab]')];
  const panels = {
    send: document.getElementById('panel-send'),
    sync: document.getElementById('panel-sync')
  };
  let activeTab = 'send';

  function selectTab(id, focus = false) {
    if (!panels[id]) return;
    activeTab = id;
    tabs.forEach((tab) => {
      const selected = tab.dataset.codeTab === id;
      tab.setAttribute('aria-selected', String(selected));
      tab.tabIndex = selected ? 0 : -1;
      if (selected && focus) tab.focus();
    });
    Object.entries(panels).forEach(([name, panel]) => { panel.hidden = name !== id; });
  }

  tabs.forEach((tab, index) => {
    tab.addEventListener('click', () => selectTab(tab.dataset.codeTab));
    tab.addEventListener('keydown', (event) => {
      if (!['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(event.key)) return;
      event.preventDefault();
      const nextIndex = event.key === 'Home' ? 0 : event.key === 'End' ? tabs.length - 1 :
        (index + (event.key === 'ArrowRight' ? 1 : -1) + tabs.length) % tabs.length;
      selectTab(tabs[nextIndex].dataset.codeTab, true);
    });
  });

  const copyButton = document.getElementById('copy-code');
  if (copyButton) {
    copyButton.addEventListener('click', async () => {
      const rawCode = panels[activeTab].querySelector('code').textContent.trim();
      let success = false;
      try {
        if (navigator.clipboard && window.isSecureContext) {
          await navigator.clipboard.writeText(rawCode);
          success = true;
        } else {
          const temporary = document.createElement('textarea');
          temporary.value = rawCode;
          temporary.setAttribute('readonly', '');
          temporary.style.position = 'fixed';
          temporary.style.opacity = '0';
          document.body.appendChild(temporary);
          temporary.select();
          success = document.execCommand('copy');
          temporary.remove();
        }
      } catch (_) { success = false; }
      const label = copyButton.querySelector('span');
      label.textContent = success ? 'Copied!' : 'Select & copy';
      announce(success ? 'Code example copied to clipboard' : 'Automatic copy unavailable. Select the code manually.');
      window.setTimeout(() => { label.textContent = 'Copy'; }, 2200);
    });
  }

  // A local, explicitly simulated version of the event-stream/cursor model.
  const demoToggle = document.getElementById('demo-toggle');
  const demoPublish = document.getElementById('demo-publish');
  const demoReset = document.getElementById('demo-reset');
  const demoConnection = document.getElementById('demo-connection');
  const demoCursor = document.getElementById('demo-cursor');
  const demoQueue = document.getElementById('demo-queue-label');
  const demoEventList = document.getElementById('demo-event-list');
  let connected = true;
  let currentSequence = 40;
  let cursor = 40;
  let pending = [];
  let sentCount = 0;
  const samples = ['A new event was published.', 'Your agent sent a reply.', 'A device reported its status.', 'Another participant joined in.'];

  function appendClientEvent(event) {
    const row = document.createElement('div');
    row.className = 'demo-event';
    const seq = document.createElement('span');
    seq.className = 'event-seq';
    seq.textContent = `#${event.sequence}`;
    const text = document.createElement('span');
    text.textContent = event.message;
    const check = document.createElement('span');
    check.className = 'event-check';
    check.setAttribute('aria-hidden', 'true');
    check.textContent = '✓';
    row.append(seq, text, check);
    demoEventList.appendChild(row);
    while (demoEventList.children.length > 7) demoEventList.firstElementChild.remove();
    demoEventList.scrollTop = demoEventList.scrollHeight;
    cursor = event.sequence;
    demoCursor.textContent = `cursor: ${cursor}`;
  }

  function updateDemo() {
    demoConnection.classList.toggle('offline', !connected);
    demoConnection.querySelector('span').textContent = connected ? 'CONNECTED' : 'OFFLINE';
    demoToggle.textContent = connected ? 'Go offline' : 'Reconnect & sync';
    demoQueue.textContent = pending.length ?
      `${pending.length} stored event${pending.length === 1 ? '' : 's'} awaiting sync` :
      (connected ? 'All events synchronized' : 'Client offline · server still accepting events');
  }

  if (demoToggle && demoPublish && demoReset) {
    demoToggle.addEventListener('click', () => {
      connected = !connected;
      if (connected) {
        const count = pending.length;
        pending.forEach(appendClientEvent);
        pending = [];
        announce(count ? `Reconnected. Recovered ${count} stored event${count === 1 ? '' : 's'}.` : 'Reconnected. No events to recover.');
      } else {
        announce('Client is offline. You can publish events to the simulated server.');
      }
      updateDemo();
    });

    demoPublish.addEventListener('click', () => {
      currentSequence += 1;
      const event = { sequence: currentSequence, message: samples[sentCount++ % samples.length] };
      if (connected) {
        appendClientEvent(event);
        announce(`Published event ${currentSequence}, delivered to the connected client.`);
      } else {
        pending.push(event);
        announce(`Published event ${currentSequence}, stored on the simulated server for later sync.`);
      }
      updateDemo();
    });

    demoReset.addEventListener('click', () => {
      connected = true;
      currentSequence = 40;
      cursor = 40;
      pending = [];
      sentCount = 0;
      demoCursor.textContent = 'cursor: 40';
      demoEventList.replaceChildren();
      const initial = document.createElement('div');
      initial.className = 'demo-event demo-event-initial';
      initial.innerHTML = '<span class="event-seq">#40</span><span>Connected. Waiting for events.</span><span class="event-check" aria-hidden="true">✓</span>';
      demoEventList.appendChild(initial);
      updateDemo();
      announce('Simulation reset. Client connected at cursor 40.');
    });
    updateDemo();
  }

  const year = document.getElementById('year');
  if (year) year.textContent = new Date().getFullYear();
})();
