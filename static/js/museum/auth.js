const AuthModule = (() => {
    let currentUser = null; // { id, email, display_name }

    async function init() {
        try {
            const res = await fetch('/auth/me', { credentials: 'same-origin' });
            if (res.ok) {
                currentUser = await res.json();
                renderAccountUI();
            } else if (res.status === 401) {
                // Not authenticated — redirect to login page.
                window.location.href = '/login';
            } else {
                // Other error — hide account UI silently.
                const dropdown = document.getElementById('account-dropdown');
                if (dropdown) dropdown.style.display = 'none';
            }
        } catch (err) {
            // Network error — silently ignore, hide account UI
            const dropdown = document.getElementById('account-dropdown');
            if (dropdown) dropdown.style.display = 'none';
        }
    }

    function renderAccountUI() {
        const dropdown = document.getElementById('account-dropdown');
        if (!dropdown) return;

        // Show the account dropdown
        dropdown.style.display = '';

        // Set display name — visitors see "Visitor" rather than the archive owner's name
        const nameEl = document.getElementById('account-display-name');
        if (nameEl && currentUser) {
            nameEl.textContent = currentUser.is_visitor ? 'Visitor' : (currentUser.display_name || currentUser.email || '');
        }

        // Wire dropdown trigger toggle
        const trigger = document.getElementById('account-dropdown-trigger');
        if (trigger) {
            trigger.addEventListener('click', (e) => {
                e.stopPropagation();
                const menu = document.getElementById('account-dropdown-menu');
                if (menu) menu.style.display = menu.style.display === 'none' ? 'block' : 'none';
            });
            document.addEventListener('click', () => {
                const menu = document.getElementById('account-dropdown-menu');
                if (menu) menu.style.display = 'none';
            });
        }

        // Wire logout button
        const logoutBtn = document.getElementById('account-logout-btn');
        if (logoutBtn) {
            logoutBtn.addEventListener('click', logout);
        }
    }

    async function logout() {
        try {
            await fetch('/auth/logout', { method: 'POST', credentials: 'same-origin' });
        } catch (err) {
            // Ignore errors — redirect regardless
        }
        window.location.href = '/login';
    }

    function getUser() {
        return currentUser;
    }

    // Self-initialize (loaded after app.js, so Modals.initAll() has already run)
    init();

    return { init, getUser, logout };
})();
