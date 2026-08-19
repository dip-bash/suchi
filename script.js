document.addEventListener('DOMContentLoaded', () => {
    // --- Commands Tab Logic ---
    const copyBtn = document.getElementById('copy-btn');
    const installCommandEl = document.getElementById('install-command');
    const cmdTabs = document.querySelectorAll('.cmd-tab');

    cmdTabs.forEach(tab => {
        tab.addEventListener('click', () => {
            cmdTabs.forEach(t => {
                t.classList.remove('active');
                t.setAttribute('aria-selected', 'false');
            });
            tab.classList.add('active');
            tab.setAttribute('aria-selected', 'true');
            installCommandEl.innerText = tab.getAttribute('data-cmd');
        });
    });

    async function copyToClipboardFallback(text) {
        if (navigator.clipboard && window.isSecureContext) {
            try {
                await navigator.clipboard.writeText(text);
                return true;
            } catch (err) {
                console.warn('Clipboard API failed, falling back to execCommand', err);
            }
        }

        const textArea = document.createElement('textarea');
        textArea.value = text;
        textArea.style.top = '0';
        textArea.style.left = '0';
        textArea.style.position = 'fixed';
        textArea.style.opacity = '0';

        document.body.appendChild(textArea);
        textArea.focus();
        textArea.select();

        let succeeded = false;
        try {
            succeeded = document.execCommand('copy');
        } catch (err) {
            console.error('Fallback: unable to copy', err);
        }

        document.body.removeChild(textArea);
        return succeeded;
    }

    let copyResetTimer = null;
    copyBtn.addEventListener('click', async () => {
        try {
            await copyToClipboardFallback(installCommandEl.innerText);
            clearTimeout(copyResetTimer);
            const originalText = copyBtn.innerText;
            copyBtn.innerText = 'Copied!';
            copyBtn.classList.add('copied');
            copyResetTimer = setTimeout(() => {
                copyBtn.innerText = originalText;
                copyBtn.classList.remove('copied');
            }, 2000);
        } catch (err) {
            console.error('Failed to copy text: ', err);
        }
    });

    // --- Interactive Terminal Demo Logic ---
    const mockData = {
        0: [ // Clipboard History
            { text: 'sudo pacman -Syu linux-firmware', time: '1m ago', pinned: true },
            { text: 'systemctl --user status suchi-daemon.service', time: '2m ago', pinned: false },
            { text: 'curl -sSL https://raw.githubusercontent.com/dip-bash/suchi/main/install.sh | bash', time: '3m ago', pinned: false },
            { text: 'Lorem ipsum dolor sit amet, consectetur adipiscing elit.', time: '1h ago', pinned: false }
        ],
        1: [ // Notes Scratchpad
            { text: 'Server config: evaluate Nginx routing', time: 'Today', pinned: true },
            { text: 'Meeting notes: adjust Neovim 0.12.2 native completion', time: 'Yesterday', pinned: false },
            { text: 'Fix shell script piping into Playwright scraper', time: '2d ago', pinned: false }
        ],
        2: [ // Text Expander
            { text: ':mail -> saumyadip@example.com', time: 'Active', pinned: false },
            { text: ':gh -> https://github.com/dip-bash', time: 'Active', pinned: false },
            { text: ':shrug -> ¯\\_(ツ)_/¯', time: 'Active', pinned: true }
        ]
    };

    let currentTab = 0; // 0: Clipboard, 1: Notes, 2: Expander
    let mode = 'NORMAL'; // 'NORMAL' or 'INSERT'
    let selectedIndex = 0;
    let filteredData = [];
    let showHelp = false;

    const searchInput = document.getElementById('demo-search');
    const demoList = document.getElementById('demo-list');
    const demoCount = document.getElementById('demo-count');
    const terminalWindow = document.getElementById('demo-terminal');
    const modeBadge = document.getElementById('mode-badge');
    const helpOverlay = document.getElementById('help-overlay');
    const tabEls = [
        document.getElementById('tab-0'),
        document.getElementById('tab-1'),
        document.getElementById('tab-2')
    ];
    const actionCopyBtn = document.getElementById('action-copy');
    const actionPinBtn = document.getElementById('action-pin');
    const actionDeleteBtn = document.getElementById('action-delete');
    const actionHelpBtn = document.getElementById('action-help');

    function updateData() {
        const query = searchInput.value;
        const data = [...mockData[currentTab]];

        // Sort: pinned items appear first, preserving relative order otherwise
        data.sort((a, b) => (b.pinned === a.pinned) ? 0 : (b.pinned ? 1 : -1));

        filteredData = query ? data.filter(item => fuzzyMatch(query, item.text)) : data;

        if (selectedIndex >= filteredData.length) {
            selectedIndex = Math.max(0, filteredData.length - 1);
        }
    }

    function fuzzyMatch(pattern, str) {
        pattern = pattern.toLowerCase();
        str = str.toLowerCase();
        let patternIdx = 0;
        let strIdx = 0;
        while (patternIdx < pattern.length && strIdx < str.length) {
            if (pattern[patternIdx] === str[strIdx]) patternIdx++;
            strIdx++;
        }
        return patternIdx === pattern.length;
    }

    function renderTabs() {
        tabEls.forEach((el, idx) => {
            const isActive = idx === currentTab;
            el.classList.toggle('active', isActive);
            el.setAttribute('aria-selected', String(isActive));
        });
    }

    function renderMode() {
        if (mode === 'NORMAL') {
            modeBadge.innerText = 'NORMAL';
            modeBadge.className = 'mode-badge normal-mode';
            searchInput.readOnly = true;
        } else {
            modeBadge.innerText = 'INSERT';
            modeBadge.className = 'mode-badge insert-mode';
            searchInput.readOnly = false;
        }
    }

    function renderActionState() {
        const hasSelection = filteredData.length > 0 && !!filteredData[selectedIndex];
        actionCopyBtn.disabled = !hasSelection;
        actionDeleteBtn.disabled = !hasSelection;
        actionPinBtn.disabled = !hasSelection;
        actionPinBtn.classList.toggle('is-active', hasSelection && !!filteredData[selectedIndex].pinned);
    }

    function renderList() {
        updateData();
        renderTabs();
        renderMode();
        renderActionState();

        helpOverlay.classList.toggle('hidden', !showHelp);

        demoList.innerHTML = '';
        demoCount.innerText = `${filteredData.length} item${filteredData.length === 1 ? '' : 's'}`;

        if (filteredData.length === 0) {
            const li = document.createElement('li');
            li.className = 'terminal-item terminal-item-empty';
            li.innerText = 'No matches found / Empty';
            demoList.appendChild(li);
            return;
        }

        const fragment = document.createDocumentFragment();

        filteredData.forEach((item, index) => {
            const li = document.createElement('li');
            li.className = `terminal-item ${index === selectedIndex ? 'selected' : ''}`;
            li.setAttribute('role', 'option');
            li.setAttribute('aria-selected', String(index === selectedIndex));

            const textSpan = document.createElement('span');
            textSpan.className = 'item-text';

            if (index === selectedIndex) {
                const arrow = document.createElement('span');
                arrow.className = 'item-arrow';
                arrow.setAttribute('aria-hidden', 'true');
                arrow.innerText = '\u2192';
                textSpan.appendChild(arrow);
            }
            textSpan.appendChild(document.createTextNode(item.text));

            const metaSpan = document.createElement('span');
            metaSpan.className = 'item-meta';

            if (item.pinned) {
                const pin = document.createElement('span');
                pin.className = 'pin-icon';
                pin.setAttribute('aria-label', 'Pinned');
                pin.innerText = '\uD83D\uDCCC';
                metaSpan.appendChild(pin);
            }

            const timeSpan = document.createElement('span');
            timeSpan.className = 'time-ago';
            timeSpan.innerText = item.time;
            metaSpan.appendChild(timeSpan);

            li.appendChild(textSpan);
            li.appendChild(metaSpan);

            li.addEventListener('click', () => {
                selectedIndex = index;
                renderList();
            });

            fragment.appendChild(li);
        });

        demoList.appendChild(fragment);

        const selectedEl = demoList.querySelector('.selected');
        if (selectedEl) {
            selectedEl.scrollIntoView({ block: 'nearest' });
        }
    }

    function enterInsertMode() {
        mode = 'INSERT';
        renderList();
        searchInput.focus();
    }

    function exitInsertMode() {
        mode = 'NORMAL';
        renderList();
        terminalWindow.focus();
    }

    function toggleHelp() {
        showHelp = !showHelp;
        renderList();
    }

    function togglePinSelected() {
        const activeItem = filteredData[selectedIndex];
        if (!activeItem) return;
        activeItem.pinned = !activeItem.pinned;
        renderList();
    }

    function deleteSelected() {
        const activeItem = filteredData[selectedIndex];
        if (!activeItem) return;
        const globalIdx = mockData[currentTab].indexOf(activeItem);
        if (globalIdx > -1) {
            mockData[currentTab].splice(globalIdx, 1);
        }
        renderList();
    }

    async function performCopy() {
        const activeItem = filteredData[selectedIndex];
        if (!activeItem) return;

        let textToCopy = activeItem.text;
        if (currentTab === 2) {
            // For Text Expander, copy just the expanded payload segment
            textToCopy = textToCopy.split('->')[1]?.trim() || textToCopy;
        }

        try {
            await copyToClipboardFallback(textToCopy);
            const originalPlaceholder = searchInput.placeholder;
            searchInput.value = '';
            searchInput.placeholder = 'Copied to clipboard!';
            searchInput.classList.add('copied-flash');
            setTimeout(() => {
                searchInput.placeholder = originalPlaceholder;
                searchInput.classList.remove('copied-flash');
            }, 1500);
        } catch (err) {
            console.error('Failed to copy', err);
        }
    }

    function switchTab(idx) {
        currentTab = idx;
        selectedIndex = 0;
        searchInput.value = '';
        renderList();
    }

    searchInput.addEventListener('input', () => {
        selectedIndex = 0;
        renderList();
    });

    // Tapping the search bar while in NORMAL mode is the touch-friendly
    // equivalent of pressing "i" on a keyboard.
    searchInput.addEventListener('focus', () => {
        if (mode === 'NORMAL') {
            enterInsertMode();
        }
    });

    terminalWindow.addEventListener('keydown', async (e) => {
        // Tab cycling handler
        if (e.key === 'Tab') {
            e.preventDefault();
            switchTab((currentTab + 1) % 3);
            return;
        }

        // Help overlay handler
        if (showHelp && e.key !== '?') {
            if (e.key === 'Escape' || e.key === 'Enter') {
                showHelp = false;
                renderList();
            }
            return;
        }

        if (mode === 'NORMAL') {
            if (e.key === 'j' || e.key === 'ArrowDown') {
                e.preventDefault();
                if (selectedIndex < filteredData.length - 1) {
                    selectedIndex++;
                    renderList();
                }
            } else if (e.key === 'k' || e.key === 'ArrowUp') {
                e.preventDefault();
                if (selectedIndex > 0) {
                    selectedIndex--;
                    renderList();
                }
            } else if (e.key === 'i') {
                e.preventDefault();
                enterInsertMode();
            } else if (e.key === 'p') {
                e.preventDefault();
                togglePinSelected();
            } else if (e.key === 'd') {
                e.preventDefault();
                deleteSelected();
            } else if (e.key === '?') {
                e.preventDefault();
                toggleHelp();
            } else if (e.key === 'Enter' || e.key === 'y') {
                e.preventDefault();
                await performCopy();
            }
        } else if (mode === 'INSERT') {
            if (e.key === 'Escape') {
                e.preventDefault();
                exitInsertMode();
            } else if (e.key === 'Enter') {
                e.preventDefault();
                await performCopy();
                exitInsertMode();
            } else if (e.key === 'ArrowDown') {
                if (selectedIndex < filteredData.length - 1) {
                    selectedIndex++;
                    renderList();
                }
            } else if (e.key === 'ArrowUp') {
                if (selectedIndex > 0) {
                    selectedIndex--;
                    renderList();
                }
            }
        }
    });

    tabEls.forEach((el, idx) => {
        el.addEventListener('click', () => switchTab(idx));
    });

    // Touch/click action bar — same actions as the keyboard shortcuts,
    // exposed as tappable buttons for phones and tablets.
    actionCopyBtn.addEventListener('click', () => performCopy());
    actionPinBtn.addEventListener('click', () => togglePinSelected());
    actionDeleteBtn.addEventListener('click', () => deleteSelected());
    actionHelpBtn.addEventListener('click', () => toggleHelp());

    renderList();

    // Fetch GitHub star count
    async function fetchGitHubStars() {
        const starCountEl = document.getElementById('github-stars');
        try {
            const response = await fetch('https://api.github.com/repos/dip-bash/suchi');
            if (response.ok) {
                const data = await response.json();
                if (starCountEl && typeof data.stargazers_count === 'number') {
                    starCountEl.innerText = data.stargazers_count.toLocaleString();
                }
            } else if (starCountEl) {
                starCountEl.innerText = '—';
            }
        } catch (error) {
            console.error('Failed to fetch GitHub stars:', error);
            if (starCountEl) {
                starCountEl.innerText = '—';
            }
        }
    }

    fetchGitHubStars();
});
