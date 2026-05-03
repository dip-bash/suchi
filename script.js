document.addEventListener('DOMContentLoaded', () => {
    // --- Commands Tab Logic ---
    const copyBtn = document.getElementById('copy-btn');
    const installCommandEl = document.getElementById('install-command');
    const cmdTabs = document.querySelectorAll('.cmd-tab');

    cmdTabs.forEach(tab => {
        tab.addEventListener('click', () => {
            cmdTabs.forEach(t => t.classList.remove('active'));
            tab.classList.add('active');
            installCommandEl.innerText = tab.getAttribute('data-cmd');
        });
    });

    async function copyToClipboardFallback(text) {
        if (navigator.clipboard && window.isSecureContext) {
            try {
                await navigator.clipboard.writeText(text);
                return;
            } catch (err) {
                console.warn('Clipboard API failed, falling back to execCommand', err);
            }
        }
        
        // Fallback for file:// or unsecure contexts
        const textArea = document.createElement("textarea");
        textArea.value = text;
        // Avoid scrolling to bottom
        textArea.style.top = "0";
        textArea.style.left = "0";
        textArea.style.position = "fixed";
        textArea.style.opacity = "0";
        
        document.body.appendChild(textArea);
        textArea.focus();
        textArea.select();
        
        try {
            document.execCommand('copy');
        } catch (err) {
            console.error('Fallback: Oops, unable to copy', err);
        }
        
        document.body.removeChild(textArea);
    }

    copyBtn.addEventListener('click', async () => {
        try {
            await copyToClipboardFallback(installCommandEl.innerText);
            const originalText = copyBtn.innerText;
            copyBtn.innerText = 'Copied!';
            copyBtn.style.backgroundColor = '#fafafa';
            copyBtn.style.color = '#000000';
            setTimeout(() => {
                copyBtn.innerText = originalText;
                copyBtn.style.backgroundColor = '';
                copyBtn.style.color = '';
            }, 2000);
        } catch (err) {
            console.error('Failed to copy text: ', err);
        }
    });

    // --- Interactive Terminal Demo Logic ---
    const mockData = [
        { text: 'curl -sSL https://raw.githubusercontent.com/dip-bash/suchi/main/install.sh | bash', time: 'Just now' },
        { text: 'curl -sSL https://raw.githubusercontent.com/dip-bash/suchi/main/uninstall.sh | bash', time: '1m ago' },
        { text: 'gnome-terminal --geometry=80x20 -- suchi', time: '2m ago' },
        { text: 'kitty -o initial_window_width=80c -o initial_window_height=20c -o remember_window_size=no -- suchi', time: '3m ago' },
        { text: 'Lorem ipsum dolor sit amet, consectetur adipiscing elit.', time: '1h ago' },
        { text: 'Sed do eiusmod tempor incididunt ut labore et dolore magna aliqua.', time: '2h ago' },
        { text: 'Ut enim ad minim veniam, quis nostrud exercitation ullamco laboris.', time: '3h ago' },
        { text: 'Nisi ut aliquip ex ea commodo consequat. Duis aute irure dolor.', time: '4h ago' },
        { text: 'In reprehenderit in voluptate velit esse cillum dolore eu fugiat.', time: '5h ago' },
        { text: 'Nulla pariatur. Excepteur sint occaecat cupidatat non proident.', time: '6h ago' },
        { text: 'Sunt in culpa qui officia deserunt mollit anim id est laborum.', time: '1d ago' },
        { text: 'Curabitur pretium tincidunt lacus. Nulla gravida orci a odio.', time: '2d ago' },
        { text: 'Nullam varius, turpis et commodo pharetra, est eros bibendum elit.', time: '3d ago' }
    ];

    const searchInput = document.getElementById('demo-search');
    const demoList = document.getElementById('demo-list');
    const demoCount = document.getElementById('demo-count');
    const terminalWindow = document.getElementById('demo-terminal');
    
    let filteredData = [...mockData];
    let selectedIndex = 0;

    function renderList() {
        demoList.innerHTML = '';
        demoCount.innerText = `${filteredData.length} items`;

        if (filteredData.length === 0) {
            const li = document.createElement('li');
            li.className = 'terminal-item';
            li.innerText = 'No matches found';
            demoList.appendChild(li);
            return;
        }

        filteredData.forEach((item, index) => {
            const li = document.createElement('li');
            li.className = `terminal-item ${index === selectedIndex ? 'selected' : ''}`;
            
            const textSpan = document.createElement('span');
            textSpan.className = 'item-text';
            textSpan.innerText = item.text;
            
            const timeSpan = document.createElement('span');
            timeSpan.className = 'time-ago';
            timeSpan.innerText = item.time;

            if (index === selectedIndex) {
                textSpan.innerHTML = `<span style="color: var(--text-primary); margin-right: 8px;">&rarr;</span> ${item.text}`;
            }

            li.appendChild(textSpan);
            li.appendChild(timeSpan);
            
            li.addEventListener('click', () => {
                selectedIndex = index;
                renderList();
            });

            demoList.appendChild(li);
        });

        const selectedEl = demoList.querySelector('.selected');
        if (selectedEl) {
            selectedEl.scrollIntoView({ block: 'nearest' });
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

    searchInput.addEventListener('input', (e) => {
        const query = e.target.value;
        if (!query) {
            filteredData = [...mockData];
        } else {
            filteredData = mockData.filter(item => fuzzyMatch(query, item.text));
        }
        selectedIndex = 0;
        renderList();
    });

    terminalWindow.addEventListener('keydown', async (e) => {
        if (e.key === 'ArrowDown') {
            e.preventDefault();
            if (selectedIndex < filteredData.length - 1) {
                selectedIndex++;
                renderList();
            }
        } else if (e.key === 'ArrowUp') {
            e.preventDefault();
            if (selectedIndex > 0) {
                selectedIndex--;
                renderList();
            }
        } else if (e.key === 'Enter') {
            e.preventDefault();
            if (filteredData[selectedIndex]) {
                const textToCopy = filteredData[selectedIndex].text;
                try {
                    await copyToClipboardFallback(textToCopy);
                    const originalPlaceholder = searchInput.placeholder;
                    const originalValue = searchInput.value;
                    searchInput.value = '';
                    searchInput.placeholder = 'Copied to clipboard!';
                    searchInput.style.color = '#fafafa'; // Highlight color
                    setTimeout(() => {
                        searchInput.placeholder = originalPlaceholder;
                        searchInput.value = originalValue;
                        searchInput.style.color = 'var(--text-primary)';
                    }, 1500);
                } catch (err) {
                    console.error('Failed to copy', err);
                }
            }
        } else if (e.key === 'Escape') {
            searchInput.blur();
            terminalWindow.blur();
        } else {
            searchInput.focus();
        }
    });

    // Handle terminal focus effect
    terminalWindow.addEventListener('focus', () => {
        searchInput.focus();
    });

    renderList();

    // Fetch GitHub Stars
    async function fetchGitHubStars() {
        try {
            const response = await fetch('https://api.github.com/repos/dip-bash/suchi');
            if (response.ok) {
                const data = await response.json();
                const starCountEl = document.getElementById('github-stars');
                if (starCountEl) {
                    starCountEl.innerText = data.stargazers_count.toLocaleString();
                }
            }
        } catch (error) {
            console.error('Failed to fetch GitHub stars:', error);
        }
    }

    fetchGitHubStars();
});