'use strict';

/**
 * UploadImport — handles web-based data import via file upload.
 *
 * Two flows:
 *  1. ZIP archive upload  → POST /import/upload → SSE /import/upload/stream
 *  2. Photo batch upload  → batched POST /import/photo-batch
 *
 * Self-initialises because it is loaded after app.js (defer order).
 */
const UploadImport = (() => {
    // ── ZIP upload state ──────────────────────────────────────────────────────
    let zipFile = null;
    let zipEventSource = null;
    let zipUploading = false;

    // ── Photo upload state ────────────────────────────────────────────────────
    const PHOTO_BATCH_SIZE = 10;
    let photoFiles = [];
    let photoUploading = false;

    // ── Helpers ───────────────────────────────────────────────────────────────

    function fmtBytes(n) {
        if (n < 1024) return n + ' B';
        if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
        return (n / (1024 * 1024)).toFixed(1) + ' MB';
    }

    function setProgress(barEl, pct) {
        if (barEl) barEl.style.width = Math.min(100, Math.max(0, pct)) + '%';
    }

    function showResult(el, ok, html) {
        if (!el) return;
        el.style.display = 'block';
        el.style.backgroundColor = ok ? 'rgba(16,185,129,0.1)' : 'rgba(239,68,68,0.1)';
        el.style.border = ok ? '1px solid rgba(16,185,129,0.4)' : '1px solid rgba(239,68,68,0.4)';
        el.style.color = ok ? '#065f46' : '#991b1b';
        el.innerHTML = html;
    }

    function closeZipSSE() {
        if (zipEventSource) {
            zipEventSource.close();
            zipEventSource = null;
        }
    }

    // ── ZIP modal ─────────────────────────────────────────────────────────────

    function initZipModal() {
        const modal = document.getElementById('upload-zip-modal');
        if (!modal) return;

        const closeBtn   = document.getElementById('close-upload-zip-modal');
        const cancelBtn  = document.getElementById('upload-zip-cancel-btn');
        const startBtn   = document.getElementById('upload-zip-start-btn');
        const fileInput  = document.getElementById('upload-zip-file');
        const dropzone   = document.getElementById('upload-zip-dropzone');
        const dropText   = document.getElementById('upload-zip-drop-text');
        const filenameEl = document.getElementById('upload-zip-filename');
        const typeSelect = document.getElementById('upload-zip-type');
        const progressWrap = document.getElementById('upload-zip-progress-wrap');
        const progressBar  = document.getElementById('upload-zip-progress-bar');
        const progressLabel = document.getElementById('upload-zip-progress-label');
        const statusEl    = document.getElementById('upload-zip-status');
        const resultEl    = document.getElementById('upload-zip-result');

        function resetZipModal() {
            zipFile = null;
            zipUploading = false;
            closeZipSSE();
            if (fileInput) fileInput.value = '';
            if (dropText) dropText.style.display = '';
            if (filenameEl) { filenameEl.textContent = ''; filenameEl.style.display = 'none'; }
            if (progressWrap) progressWrap.style.display = 'none';
            if (progressBar) progressBar.style.width = '0%';
            if (statusEl) statusEl.textContent = '';
            if (resultEl) { resultEl.style.display = 'none'; resultEl.textContent = ''; }
            if (startBtn) { startBtn.disabled = false; startBtn.innerHTML = '<i class="fas fa-upload"></i> Upload &amp; Import'; }
        }

        function closeModal() {
            if (zipUploading) return; // don't close mid-upload
            resetZipModal();
            modal.style.display = 'none';
        }

        function setFile(f) {
            if (!f || !f.name.toLowerCase().endsWith('.zip')) {
                if (f) showResult(resultEl, false, 'Only .zip files are accepted.');
                return;
            }
            zipFile = f;
            if (resultEl) { resultEl.style.display = 'none'; }
            if (dropText) dropText.style.display = 'none';
            if (filenameEl) {
                filenameEl.textContent = f.name + ' (' + fmtBytes(f.size) + ')';
                filenameEl.style.display = 'block';
            }
        }

        // Drag-and-drop
        if (dropzone) {
            dropzone.addEventListener('click', () => { if (!zipUploading) fileInput.click(); });
            dropzone.addEventListener('dragover', (e) => { e.preventDefault(); dropzone.classList.add('upload-import-dropzone-hover'); });
            dropzone.addEventListener('dragleave', () => { dropzone.classList.remove('upload-import-dropzone-hover'); });
            dropzone.addEventListener('drop', (e) => {
                e.preventDefault();
                dropzone.classList.remove('upload-import-dropzone-hover');
                const f = e.dataTransfer.files[0];
                if (f) setFile(f);
            });
        }

        if (fileInput) {
            fileInput.addEventListener('change', () => {
                if (fileInput.files.length) setFile(fileInput.files[0]);
            });
        }

        // Browse link inside dropzone text
        const browseLink = dropzone && dropzone.querySelector('.upload-import-browse-link');
        if (browseLink) {
            browseLink.addEventListener('click', (e) => { e.stopPropagation(); fileInput.click(); });
        }

        if (closeBtn) closeBtn.addEventListener('click', closeModal);
        if (cancelBtn) cancelBtn.addEventListener('click', closeModal);
        modal.addEventListener('click', (e) => { if (e.target === modal) closeModal(); });

        if (startBtn) {
            startBtn.addEventListener('click', async () => {
                if (zipUploading) return;
                if (!zipFile) {
                    showResult(resultEl, false, 'Please select a ZIP file first.');
                    return;
                }

                const importType = typeSelect ? typeSelect.value : 'facebook';

                zipUploading = true;
                startBtn.disabled = true;
                startBtn.innerHTML = '<i class="fas fa-spinner fa-spin"></i> Uploading…';
                if (progressWrap) progressWrap.style.display = 'block';
                if (resultEl) resultEl.style.display = 'none';
                if (progressLabel) progressLabel.textContent = 'Uploading ZIP…';
                if (statusEl) statusEl.textContent = '';

                // Build form data
                const fd = new FormData();
                fd.append('type', importType);
                fd.append('file', zipFile, zipFile.name);

                // Upload with XHR so we can track progress
                const xhr = new XMLHttpRequest();
                xhr.open('POST', '/import/upload');
                xhr.withCredentials = true;

                xhr.upload.addEventListener('progress', (e) => {
                    if (e.lengthComputable) {
                        const pct = (e.loaded / e.total) * 100;
                        setProgress(progressBar, pct);
                        if (progressLabel) progressLabel.textContent = `Uploading… ${fmtBytes(e.loaded)} / ${fmtBytes(e.total)}`;
                    }
                });

                xhr.addEventListener('load', () => {
                    if (xhr.status === 409) {
                        zipUploading = false;
                        startBtn.disabled = false;
                        startBtn.innerHTML = '<i class="fas fa-upload"></i> Upload &amp; Import';
                        showResult(resultEl, false, 'Another import is already running. Please wait for it to finish.');
                        if (progressWrap) progressWrap.style.display = 'none';
                        return;
                    }
                    if (xhr.status < 200 || xhr.status >= 300) {
                        let msg = 'Upload failed (HTTP ' + xhr.status + ')';
                        try { msg = JSON.parse(xhr.responseText).detail || msg; } catch (_) {}
                        zipUploading = false;
                        startBtn.disabled = false;
                        startBtn.innerHTML = '<i class="fas fa-upload"></i> Upload &amp; Import';
                        showResult(resultEl, false, msg);
                        if (progressWrap) progressWrap.style.display = 'none';
                        return;
                    }

                    // Upload accepted and import job has started — close the dialog
                    // and hand SSE tracking to the main import status system.
                    zipUploading = false;
                    closeZipSSE();
                    const importTypeLabel = typeSelect ? typeSelect.options[typeSelect.selectedIndex].text : 'archive';
                    resetZipModal();
                    modal.style.display = 'none';
                    if (window.ImportControls && window.ImportControls.attachUploadStream) {
                        window.ImportControls.attachUploadStream(importTypeLabel);
                    }
                });

                xhr.addEventListener('error', () => {
                    zipUploading = false;
                    startBtn.disabled = false;
                    startBtn.innerHTML = '<i class="fas fa-upload"></i> Upload &amp; Import';
                    showResult(resultEl, false, 'Network error during upload.');
                    if (progressWrap) progressWrap.style.display = 'none';
                });

                xhr.send(fd);
            });
        }

        // Wire the tiles that open this modal
        document.querySelectorAll('[data-open-modal="upload-zip-modal"]').forEach(el => {
            el.addEventListener('click', () => {
                resetZipModal();
                modal.style.display = 'flex';
            });
        });
    }

    // ── Photo upload modal ────────────────────────────────────────────────────

    function initPhotosModal() {
        const modal = document.getElementById('upload-photos-modal');
        if (!modal) return;

        const closeBtn    = document.getElementById('close-upload-photos-modal');
        const cancelBtn   = document.getElementById('upload-photos-cancel-btn');
        const startBtn    = document.getElementById('upload-photos-start-btn');
        const fileInput   = document.getElementById('upload-photos-file');
        const dropzone    = document.getElementById('upload-photos-dropzone');
        const dropText    = document.getElementById('upload-photos-drop-text');
        const countEl     = document.getElementById('upload-photos-count');
        const progressWrap  = document.getElementById('upload-photos-progress-wrap');
        const progressBar   = document.getElementById('upload-photos-progress-bar');
        const progressLabel = document.getElementById('upload-photos-progress-label');
        const statusEl      = document.getElementById('upload-photos-status');
        const resultEl      = document.getElementById('upload-photos-result');

        function resetPhotosModal() {
            photoFiles = [];
            photoUploading = false;
            if (fileInput) fileInput.value = '';
            if (dropText) dropText.style.display = '';
            if (countEl) { countEl.textContent = ''; countEl.style.display = 'none'; }
            if (progressWrap) progressWrap.style.display = 'none';
            if (progressBar) progressBar.style.width = '0%';
            if (statusEl) statusEl.textContent = '';
            if (resultEl) { resultEl.style.display = 'none'; resultEl.textContent = ''; }
            if (startBtn) { startBtn.disabled = false; startBtn.innerHTML = '<i class="fas fa-upload"></i> Upload Photos'; }
        }

        function closeModal() {
            if (photoUploading) return;
            resetPhotosModal();
            modal.style.display = 'none';
        }

        const IMAGE_TYPES = new Set(['image/jpeg', 'image/jpg', 'image/png', 'image/gif', 'image/webp', 'image/heic', 'image/heif', 'image/tiff', 'image/bmp']);

        function setFiles(files) {
            const imgs = Array.from(files).filter(f => {
                const ext = f.name.split('.').pop().toLowerCase();
                const mime = f.type.toLowerCase();
                return IMAGE_TYPES.has(mime) || ['jpg','jpeg','png','gif','webp','heic','heif','tiff','bmp'].includes(ext);
            });
            photoFiles = imgs;
            if (resultEl) resultEl.style.display = 'none';
            if (imgs.length === 0) {
                if (dropText) dropText.style.display = '';
                if (countEl) { countEl.style.display = 'none'; }
                showResult(resultEl, false, 'No image files found in the selected folder.');
                return;
            }
            if (dropText) dropText.style.display = 'none';
            if (countEl) {
                countEl.textContent = imgs.length + ' image' + (imgs.length !== 1 ? 's' : '') + ' selected';
                countEl.style.display = 'block';
            }
        }

        if (dropzone) {
            dropzone.addEventListener('click', () => { if (!photoUploading) fileInput.click(); });
            dropzone.addEventListener('dragover', (e) => { e.preventDefault(); dropzone.classList.add('upload-import-dropzone-hover'); });
            dropzone.addEventListener('dragleave', () => { dropzone.classList.remove('upload-import-dropzone-hover'); });
            dropzone.addEventListener('drop', (e) => {
                e.preventDefault();
                dropzone.classList.remove('upload-import-dropzone-hover');
                if (e.dataTransfer.files.length) setFiles(e.dataTransfer.files);
            });
        }

        if (fileInput) {
            fileInput.addEventListener('change', () => {
                if (fileInput.files.length) setFiles(fileInput.files);
            });
        }

        if (closeBtn) closeBtn.addEventListener('click', closeModal);
        if (cancelBtn) cancelBtn.addEventListener('click', closeModal);
        modal.addEventListener('click', (e) => { if (e.target === modal) closeModal(); });

        if (startBtn) {
            startBtn.addEventListener('click', async () => {
                if (photoUploading) return;
                if (photoFiles.length === 0) {
                    showResult(resultEl, false, 'Please select a folder of photos first.');
                    return;
                }

                photoUploading = true;
                startBtn.disabled = true;
                startBtn.innerHTML = '<i class="fas fa-spinner fa-spin"></i> Uploading…';
                if (progressWrap) progressWrap.style.display = 'block';
                if (resultEl) resultEl.style.display = 'none';

                const total = photoFiles.length;
                let done = 0;
                let totalImported = 0;
                let totalUpdated = 0;
                let totalErrors = 0;
                let aborted = false;

                for (let i = 0; i < photoFiles.length && !aborted; i += PHOTO_BATCH_SIZE) {
                    const batch = photoFiles.slice(i, i + PHOTO_BATCH_SIZE);
                    const fd = new FormData();
                    batch.forEach(f => fd.append('files', f, f.name));

                    if (progressLabel) progressLabel.textContent = `Uploading batch ${Math.floor(i / PHOTO_BATCH_SIZE) + 1} of ${Math.ceil(total / PHOTO_BATCH_SIZE)}…`;
                    if (statusEl) statusEl.textContent = `${done} / ${total} files sent`;
                    setProgress(progressBar, (done / total) * 100);

                    try {
                        const res = await fetch('/import/photo-batch', {
                            method: 'POST',
                            credentials: 'same-origin',
                            body: fd,
                        });
                        if (res.ok) {
                            const data = await res.json();
                            totalImported += data.imported || 0;
                            totalUpdated  += data.updated  || 0;
                            totalErrors   += data.errors   || 0;
                        } else {
                            totalErrors += batch.length;
                        }
                    } catch (_) {
                        totalErrors += batch.length;
                    }
                    done += batch.length;
                }

                setProgress(progressBar, 100);
                photoUploading = false;
                startBtn.disabled = false;
                startBtn.innerHTML = '<i class="fas fa-upload"></i> Upload Photos';

                const ok = totalErrors < total;
                showResult(resultEl, ok,
                    `<i class="fas fa-check-circle"></i> Done — ` +
                    `${totalImported} new, ${totalUpdated} updated, ${totalErrors} errors ` +
                    `(${total} files total)`
                );
                if (statusEl) statusEl.textContent = '';
            });
        }

        // Wire tiles
        document.querySelectorAll('[data-open-modal="upload-photos-modal"]').forEach(el => {
            el.addEventListener('click', () => {
                resetPhotosModal();
                modal.style.display = 'flex';
            });
        });
    }

    // ── init ──────────────────────────────────────────────────────────────────

    function init() {
        initZipModal();
        initPhotosModal();
    }

    init();

    return { init };
})();
