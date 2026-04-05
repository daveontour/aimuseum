'use strict';

/**
 * UploadImport — handles web-based data import via file upload.
 *
 * Two flows:
 *  1. ZIP archive upload  → tus resumable upload → /import/tus/ → SSE /import/upload/stream
 *  2. Photo batch upload  → batched POST /import/photo-batch
 *
 * Self-initialises because it is loaded after app.js (defer order).
 */
const UploadImport = (() => {
    // ── ZIP upload state ──────────────────────────────────────────────────────
    let zipFile = null;
    let zipEventSource = null;
    let zipUploading = false;
    let currentTusUpload = null;
    let chunkSizeMB = 10; // default; refreshed from /api/upload-config on init
    let maxUploadGB = 32; // default GiB cap for ZIP tus upload; refreshed from /api/upload-config

    // ── Photo upload state ────────────────────────────────────────────────────
    const PHOTO_BATCH_SIZE = 10;
    let photoFiles = [];
    /** Unfiltered selection (folder picker or drop); used to re-apply subfolder checkbox. */
    let photoFilesRaw = null;
    let photoUploading = false;

    function readAllDirectoryEntries(dirReader) {
        return new Promise((resolve, reject) => {
            const acc = [];
            const read = () => {
                dirReader.readEntries((batch) => {
                    if (batch.length === 0) resolve(acc);
                    else {
                        acc.push(...batch);
                        read();
                    }
                }, reject);
            };
            read();
        });
    }

    /**
     * Walks a dropped directory and records path-from-root on each File (drag-drop does not set webkitRelativePath).
     * @param {string} prefix POSIX-style path without leading slash (e.g. "Vacation/beach")
     */
    async function collectFromDirectoryRecursive(dirEntry, out, prefix) {
        const reader = dirEntry.createReader();
        const entries = await readAllDirectoryEntries(reader);
        for (const entry of entries) {
            const rel = prefix ? prefix + '/' + entry.name : entry.name;
            if (entry.isFile) {
                await new Promise((resolve, reject) => {
                    entry.file((f) => {
                        f._dmRelativePath = rel.replace(/\\/g, '/');
                        out.push(f);
                        resolve();
                    }, reject);
                });
            } else if (entry.isDirectory) {
                await collectFromDirectoryRecursive(entry, out, rel.replace(/\\/g, '/'));
            }
        }
    }

    async function collectFromDirectoryShallow(dirEntry, out) {
        const reader = dirEntry.createReader();
        const entries = await readAllDirectoryEntries(reader);
        for (const entry of entries) {
            if (entry.isFile) {
                await new Promise((resolve, reject) => {
                    entry.file((f) => {
                        f._dmRelativePath = entry.name.replace(/\\/g, '/');
                        out.push(f);
                        resolve();
                    }, reject);
                });
            }
        }
    }

    /** Relative path for multipart + server rel_paths (folder picker, drop traversal, or basename). */
    function photoUploadRelativePath(file) {
        const raw = file._dmRelativePath || file.webkitRelativePath || file.name || '';
        const p = String(raw).replace(/\\/g, '/').trim();
        return p || file.name;
    }

    /**
     * @param {DataTransfer} dt
     * @param {boolean} includeSubfolders
     */
    async function collectDroppedFiles(dt, includeSubfolders) {
        const out = [];
        let anyEntry = false;
        if (dt.items && dt.items.length) {
            for (const item of Array.from(dt.items)) {
                const entry = item.webkitGetAsEntry?.();
                if (!entry) continue;
                anyEntry = true;
                if (entry.isFile) {
                    await new Promise((resolve, reject) => {
                        entry.file((f) => {
                            f._dmRelativePath = entry.name.replace(/\\/g, '/');
                            out.push(f);
                            resolve();
                        }, reject);
                    });
                } else if (entry.isDirectory) {
                    if (includeSubfolders) await collectFromDirectoryRecursive(entry, out, '');
                    else await collectFromDirectoryShallow(entry, out);
                }
            }
        }
        if (anyEntry) return out;
        return dt.files && dt.files.length ? Array.from(dt.files) : [];
    }

    /** Display labels for ZIP import type (must match server tusPreCreate + radio values). */
    const ZIP_ARCHIVE_LABELS = {
        facebook: 'Facebook (full export)',
        instagram: 'Instagram',
        whatsapp: 'WhatsApp',
        imessage: 'iMessage / SMS (iMazing export)',
    };

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

    function fileFingerprint(f) {
        return 'tus_upload_' + f.name + '|' + f.size + '|' + f.lastModified;
    }

    /**
     * tus-js-client stores the upload URL in localStorage for resume. The server
     * returns an absolute URL (host from the POST request). If the user later
     * opens the app via a different host (e.g. 127.0.0.1 vs localhost), PATCH
     * becomes cross-origin, session cookies are not sent, the server returns
     * 401 without CORS headers, and the browser surfaces xhr.onerror as a
     * ProgressEvent with no status — the error you saw. We only persist the
     * path and always resume against the current origin.
     */
    function normalizeStoredTusUrl(raw) {
        if (!raw || typeof raw !== 'string') return undefined;
        const trimmed = raw.trim();
        if (!trimmed) return undefined;
        if (trimmed.startsWith('/')) {
            return trimmed.startsWith('/import/tus/') ? trimmed : undefined;
        }
        try {
            const u = new URL(trimmed);
            if (u.pathname.startsWith('/import/tus/')) {
                return u.pathname + u.search;
            }
        } catch (_) {
            /* ignore */
        }
        return undefined;
    }

    function persistTusUrl(fingerprint, uploadUrl) {
        if (!uploadUrl) return;
        const path = normalizeStoredTusUrl(uploadUrl);
        if (path) {
            localStorage.setItem(fingerprint, path);
        }
    }

    // ── ZIP modal ─────────────────────────────────────────────────────────────

    function initZipModal() {
        const modal = document.getElementById('upload-zip-modal');
        const confirmModal = document.getElementById('upload-zip-confirm-modal');
        if (!modal) return;

        const closeBtn    = document.getElementById('close-upload-zip-modal');
        const cancelBtn   = document.getElementById('upload-zip-cancel-btn');
        const startBtn    = document.getElementById('upload-zip-start-btn');
        const fileInput   = document.getElementById('upload-zip-file');
        const dropzone    = document.getElementById('upload-zip-dropzone');
        const dropText    = document.getElementById('upload-zip-drop-text');
        const filenameEl  = document.getElementById('upload-zip-filename');
        const progressWrap  = document.getElementById('upload-zip-progress-wrap');
        const progressBar   = document.getElementById('upload-zip-progress-bar');
        const progressLabel = document.getElementById('upload-zip-progress-label');
        const statusEl    = document.getElementById('upload-zip-status');
        const resultEl    = document.getElementById('upload-zip-result');

        const confirmCloseBtn = document.getElementById('close-upload-zip-confirm-modal');
        const confirmBackBtn = document.getElementById('upload-zip-confirm-back-btn');
        const confirmStartBtn = document.getElementById('upload-zip-confirm-start-btn');

        function abortTusUpload() {
            if (currentTusUpload) {
                currentTusUpload.abort();
                currentTusUpload = null;
            }
        }

        function resetArchiveTypeRadios() {
            const fb = document.querySelector('input[name="upload-zip-archive-type"][value="facebook"]');
            if (fb) fb.checked = true;
        }

        function getSelectedZipArchiveType() {
            const el = document.querySelector('input[name="upload-zip-archive-type"]:checked');
            const value = el && el.value ? el.value : 'facebook';
            const label = ZIP_ARCHIVE_LABELS[value] || 'archive';
            return { value, label };
        }

        function openConfirmModal() {
            if (confirmModal) confirmModal.style.display = 'flex';
        }

        function closeConfirmModal() {
            if (confirmModal) confirmModal.style.display = 'none';
        }

        function resetZipModal() {
            abortTusUpload();
            zipFile = null;
            zipUploading = false;
            closeZipSSE();
            closeConfirmModal();
            resetArchiveTypeRadios();
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
            // Allow closing even mid-upload (tus upload can be resumed later)
            abortTusUpload();
            zipUploading = false;
            resetZipModal();
            modal.style.display = 'none';
        }

        function beginZipTusUpload(importType, importTypeLabel) {
            if (!zipFile) return;

            const fingerprint = fileFingerprint(zipFile);
            const savedUrl = normalizeStoredTusUrl(localStorage.getItem(fingerprint));

            zipUploading = true;
            if (startBtn) {
                startBtn.disabled = true;
                startBtn.innerHTML = savedUrl
                    ? '<i class="fas fa-spinner fa-spin"></i> Resuming…'
                    : '<i class="fas fa-spinner fa-spin"></i> Uploading…';
            }
            if (progressWrap) progressWrap.style.display = 'block';
            if (resultEl) resultEl.style.display = 'none';
            if (progressLabel) progressLabel.textContent = savedUrl ? 'Resuming upload…' : 'Uploading ZIP…';
            if (statusEl) statusEl.textContent = '';

            const upload = new tus.Upload(zipFile, {
                endpoint: '/import/tus/',
                chunkSize: chunkSizeMB * 1024 * 1024,
                retryDelays: [0, 3000, 10000, 30000],
                metadata: { import_type: importType, filename: zipFile.name },
                uploadUrl: savedUrl,

                onProgress(bytesSent, bytesTotal) {
                    const pct = bytesTotal > 0 ? (bytesSent / bytesTotal) * 100 : 0;
                    setProgress(progressBar, pct);
                    if (progressLabel) {
                        progressLabel.textContent = `Uploading… ${fmtBytes(bytesSent)} / ${fmtBytes(bytesTotal)}`;
                    }
                },

                onSuccess() {
                    localStorage.removeItem(fingerprint);
                    currentTusUpload = null;
                    zipUploading = false;
                    closeZipSSE();
                    closeConfirmModal();
                    resetZipModal();
                    modal.style.display = 'none';
                    if (window.ImportControls && window.ImportControls.attachUploadStream) {
                        window.ImportControls.attachUploadStream(importTypeLabel, importType);
                    }
                },

                onError(err) {
                    currentTusUpload = null;
                    zipUploading = false;
                    if (startBtn) {
                        startBtn.disabled = false;
                        startBtn.innerHTML = '<i class="fas fa-upload"></i> Upload &amp; Import';
                    }
                    if (progressWrap) progressWrap.style.display = 'none';
                    let msg = err.message || 'Upload failed.';
                    if (msg.includes('403') || msg.includes('forbidden')) {
                        msg = 'Owner master unlock required. Please unlock the keyring and try again.';
                    } else if (msg.includes('409') || msg.includes('conflict')) {
                        msg = 'Another import is already running. Please wait for it to finish.';
                    } else if (msg.includes('413') || msg.includes('MAX_SIZE') || msg.includes('maximum size exceeded')) {
                        msg = `This ZIP is larger than the server allows (${maxUploadGB} GB). Set TUS_MAX_UPLOAD_GB in the server .env to raise the limit, then restart.`;
                    }
                    showResult(resultEl, false, msg);
                },

                onAfterResponse(req, res) {
                    persistTusUrl(fingerprint, upload.url);
                },
            });

            currentTusUpload = upload;
            upload.start();
        }

        function setFile(f) {
            if (!f || !f.name.toLowerCase().endsWith('.zip')) {
                if (f) showResult(resultEl, false, 'Only .zip files are accepted.');
                return;
            }
            zipFile = f;
            if (resultEl) resultEl.style.display = 'none';
            if (dropText) dropText.style.display = 'none';
            if (filenameEl) {
                filenameEl.textContent = f.name + ' (' + fmtBytes(f.size) + ')';
                filenameEl.style.display = 'block';
            }

            // Check for a resumable previous upload of this exact file.
            const saved = localStorage.getItem(fileFingerprint(f));
            if (saved && statusEl) {
                statusEl.textContent = 'Previous upload found — click Upload to resume where it left off.';
            } else if (statusEl) {
                statusEl.textContent = '';
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

        const browseLink = dropzone && dropzone.querySelector('.upload-import-browse-link');
        if (browseLink) {
            browseLink.addEventListener('click', (e) => { e.stopPropagation(); fileInput.click(); });
        }

        if (closeBtn) closeBtn.addEventListener('click', closeModal);
        if (cancelBtn) cancelBtn.addEventListener('click', closeModal);
        modal.addEventListener('click', (e) => { if (e.target === modal) closeModal(); });

        if (startBtn) {
            startBtn.addEventListener('click', () => {
                if (zipUploading) return;
                if (!zipFile) {
                    showResult(resultEl, false, 'Please select a ZIP file first.');
                    return;
                }
                if (resultEl) resultEl.style.display = 'none';
                openConfirmModal();
            });
        }

        if (confirmModal) {
            confirmModal.addEventListener('click', (e) => {
                if (e.target === confirmModal) closeConfirmModal();
            });
        }
        if (confirmCloseBtn) confirmCloseBtn.addEventListener('click', closeConfirmModal);
        if (confirmBackBtn) confirmBackBtn.addEventListener('click', closeConfirmModal);
        if (confirmStartBtn) {
            confirmStartBtn.addEventListener('click', () => {
                if (zipUploading || !zipFile) return;
                const { value, label } = getSelectedZipArchiveType();
                closeConfirmModal();
                beginZipTusUpload(value, label);
            });
        }

        function openZipModalWithArchiveType(archiveType) {
            resetZipModal();
            const r = document.querySelector('input[name="upload-zip-archive-type"][value="' + String(archiveType).replace(/"/g, '') + '"]');
            if (r) r.checked = true;
            modal.style.display = 'flex';
        }

        window.UploadImport = window.UploadImport || {};
        window.UploadImport.openZipModalWithArchiveType = openZipModalWithArchiveType;

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

        /** AbortController only for explicit Cancel; closing the dialog does not abort. */
        let photoBatchAbort = null;

        const closeBtn    = document.getElementById('close-upload-photos-modal');
        const cancelBtn   = document.getElementById('upload-photos-cancel-btn');
        const backgroundBtn = document.getElementById('upload-photos-background-btn');
        const startBtn    = document.getElementById('upload-photos-start-btn');
        const fileInput   = document.getElementById('upload-photos-file');
        const dropzone    = document.getElementById('upload-photos-dropzone');
        const dropText    = document.getElementById('upload-photos-drop-text');
        const countEl     = document.getElementById('upload-photos-count');
        const includeSubfoldersCb = document.getElementById('upload-photos-include-subfolders');
        const overwriteExistingCb = document.getElementById('upload-photos-overwrite-existing');
        const progressWrap  = document.getElementById('upload-photos-progress-wrap');
        const progressBar   = document.getElementById('upload-photos-progress-bar');
        const progressLabel = document.getElementById('upload-photos-progress-label');
        const statusEl      = document.getElementById('upload-photos-status');
        const resultEl      = document.getElementById('upload-photos-result');
        const backgroundHint = document.getElementById('upload-photos-background-hint');

        const IMAGE_TYPES = new Set(['image/jpeg', 'image/jpg', 'image/png', 'image/gif', 'image/webp', 'image/heic', 'image/heif', 'image/tiff', 'image/bmp']);
        const IMAGE_EXTS = ['jpg', 'jpeg', 'png', 'gif', 'webp', 'heic', 'heif', 'tiff', 'bmp'];

        function isImageFile(f) {
            const ext = f.name.split('.').pop().toLowerCase();
            const mime = f.type.toLowerCase();
            return IMAGE_TYPES.has(mime) || IMAGE_EXTS.includes(ext);
        }

        /** True if file is not under a subfolder relative to picked root (webkitdirectory). */
        function isImmediateChildOnly(f) {
            const p = f.webkitRelativePath;
            if (!p || typeof p !== 'string') return true;
            return !/[\\/]/.test(p);
        }

        function includeSubfoldersEnabled() {
            return !includeSubfoldersCb || includeSubfoldersCb.checked;
        }

        function applyPhotoSelection(rawArr) {
            let imgs = rawArr.filter(isImageFile);
            if (!includeSubfoldersEnabled()) {
                imgs = imgs.filter(isImmediateChildOnly);
            }
            photoFiles = imgs;
            if (resultEl) resultEl.style.display = 'none';
            if (imgs.length === 0) {
                if (dropText) dropText.style.display = '';
                if (countEl) { countEl.style.display = 'none'; }
                const hadRawImages = rawArr.some(isImageFile);
                if (hadRawImages && !includeSubfoldersEnabled()) {
                    showResult(resultEl, false, 'No images in the top-level folder. Turn on “Include photos in subfolders” or choose a folder that contains images directly inside it.');
                } else {
                    showResult(resultEl, false, 'No image files found in the selected folder.');
                }
                return;
            }
            if (dropText) dropText.style.display = 'none';
            if (countEl) {
                countEl.textContent = imgs.length + ' image' + (imgs.length !== 1 ? 's' : '') + ' selected';
                countEl.style.display = 'block';
            }
        }

        function setFilesFromRaw(filesIterable) {
            photoFilesRaw = Array.from(filesIterable);
            applyPhotoSelection(photoFilesRaw);
        }

        function clearRunningPhotoModalChrome() {
            if (backgroundHint) backgroundHint.style.display = 'none';
            modal.classList.remove('upload-photos-modal-busy');
            if (includeSubfoldersCb) includeSubfoldersCb.disabled = false;
            if (overwriteExistingCb) overwriteExistingCb.disabled = false;
            if (cancelBtn) cancelBtn.removeAttribute('title');
            if (closeBtn) closeBtn.removeAttribute('title');
            if (backgroundBtn) {
                backgroundBtn.style.display = 'none';
                backgroundBtn.removeAttribute('title');
            }
        }

        function syncRunningPhotoModalUI() {
            if (progressWrap) progressWrap.style.display = 'block';
            if (backgroundHint) backgroundHint.style.display = 'block';
            if (startBtn) {
                startBtn.disabled = true;
                startBtn.innerHTML = '<i class="fas fa-spinner fa-spin"></i> Uploading…';
            }
            modal.classList.add('upload-photos-modal-busy');
            if (includeSubfoldersCb) includeSubfoldersCb.disabled = true;
            if (overwriteExistingCb) overwriteExistingCb.disabled = true;
            if (backgroundBtn) {
                backgroundBtn.style.display = '';
                backgroundBtn.title = 'Hide this dialog; uploads keep running';
            }
            if (cancelBtn) cancelBtn.title = 'Stop upload and close';
            if (closeBtn) closeBtn.title = 'Close dialog — upload continues in the background';
        }

        function resetPhotosModal() {
            photoFiles = [];
            photoFilesRaw = null;
            photoUploading = false;
            clearRunningPhotoModalChrome();
            if (fileInput) fileInput.value = '';
            if (includeSubfoldersCb) includeSubfoldersCb.checked = true;
            if (overwriteExistingCb) overwriteExistingCb.checked = false;
            if (dropText) dropText.style.display = '';
            if (countEl) { countEl.textContent = ''; countEl.style.display = 'none'; }
            if (progressWrap) progressWrap.style.display = 'none';
            if (progressBar) progressBar.style.width = '0%';
            if (statusEl) statusEl.textContent = '';
            if (resultEl) { resultEl.style.display = 'none'; resultEl.textContent = ''; }
            if (startBtn) { startBtn.disabled = false; startBtn.innerHTML = '<i class="fas fa-upload"></i> Upload Photos'; }
        }

        /** Hide dialog; if uploading, leave the job running. */
        function dismissPhotoModal() {
            if (photoUploading) {
                modal.style.display = 'none';
                return;
            }
            resetPhotosModal();
            modal.style.display = 'none';
        }

        /** Cancel stops an in-flight upload; when idle, same as dismiss + reset. */
        function cancelPhotoModal() {
            if (photoUploading) {
                photoBatchAbort?.abort();
                modal.style.display = 'none';
                return;
            }
            resetPhotosModal();
            modal.style.display = 'none';
        }

        if (includeSubfoldersCb) {
            includeSubfoldersCb.addEventListener('change', () => {
                if (photoUploading) return;
                if (photoFilesRaw && photoFilesRaw.length) applyPhotoSelection(photoFilesRaw);
            });
        }

        if (dropzone) {
            dropzone.addEventListener('click', () => { if (!photoUploading) fileInput.click(); });
            dropzone.addEventListener('dragover', (e) => { e.preventDefault(); dropzone.classList.add('upload-import-dropzone-hover'); });
            dropzone.addEventListener('dragleave', () => { dropzone.classList.remove('upload-import-dropzone-hover'); });
            dropzone.addEventListener('drop', (e) => {
                e.preventDefault();
                dropzone.classList.remove('upload-import-dropzone-hover');
                if (photoUploading) return;
                void (async () => {
                    const raw = await collectDroppedFiles(e.dataTransfer, includeSubfoldersEnabled());
                    if (raw.length) setFilesFromRaw(raw);
                })();
            });
        }

        if (fileInput) {
            fileInput.addEventListener('change', () => {
                if (photoUploading) return;
                if (fileInput.files.length) setFilesFromRaw(fileInput.files);
            });
        }

        if (closeBtn) closeBtn.addEventListener('click', dismissPhotoModal);
        if (cancelBtn) cancelBtn.addEventListener('click', cancelPhotoModal);
        if (backgroundBtn) backgroundBtn.addEventListener('click', dismissPhotoModal);
        modal.addEventListener('click', (e) => { if (e.target === modal) dismissPhotoModal(); });

        if (startBtn) {
            startBtn.addEventListener('click', () => {
                void (async () => {
                    if (photoUploading) return;
                    if (photoFiles.length === 0) {
                        showResult(resultEl, false, 'Please select a folder of photos first.');
                        return;
                    }

                    photoUploading = true;
                    photoBatchAbort = new AbortController();
                    syncRunningPhotoModalUI();
                    if (resultEl) resultEl.style.display = 'none';

                    const total = photoFiles.length;
                    let done = 0;
                    let totalImported = 0;
                    let totalUpdated = 0;
                    let totalSkipped = 0;
                    let totalErrors = 0;
                    const overwriteVal = (overwriteExistingCb && overwriteExistingCb.checked) ? '1' : '0';

                    try {
                        for (let i = 0; i < photoFiles.length; i += PHOTO_BATCH_SIZE) {
                            const batch = photoFiles.slice(i, i + PHOTO_BATCH_SIZE);
                            const fd = new FormData();
                            fd.append('overwrite_existing', overwriteVal);
                            batch.forEach((f) => {
                                const relPath = photoUploadRelativePath(f);
                                fd.append('files', f, relPath);
                                fd.append('rel_paths', relPath);
                            });

                            if (progressLabel) {
                                progressLabel.textContent = `Uploading batch ${Math.floor(i / PHOTO_BATCH_SIZE) + 1} of ${Math.ceil(total / PHOTO_BATCH_SIZE)}…`;
                            }
                            if (statusEl) statusEl.textContent = `${done} / ${total} files sent`;
                            setProgress(progressBar, (done / total) * 100);

                            try {
                                const res = await fetch('/import/photo-batch', {
                                    method: 'POST',
                                    credentials: 'same-origin',
                                    body: fd,
                                    signal: photoBatchAbort.signal,
                                });
                                if (res.ok) {
                                    const data = await res.json();
                                    totalImported += data.imported || 0;
                                    totalUpdated += data.updated || 0;
                                    totalSkipped += data.skipped || 0;
                                    totalErrors += data.errors || 0;
                                } else {
                                    totalErrors += batch.length;
                                }
                            } catch (err) {
                                if (err && err.name === 'AbortError') {
                                    break;
                                }
                                totalErrors += batch.length;
                            }
                            done += batch.length;
                        }
                        setProgress(progressBar, 100);
                    } finally {
                        photoUploading = false;
                        photoBatchAbort = null;
                    }

                    if (statusEl) statusEl.textContent = '';

                    resetPhotosModal();
                    if (modal.style.display !== 'none') {
                        modal.style.display = 'none';
                    }

                    const savedAny = totalImported + totalUpdated > 0;
                    if (savedAny && window.ImportControls && typeof window.ImportControls.startThumbnailsAfterPhotoImport === 'function') {
                        void window.ImportControls.startThumbnailsAfterPhotoImport();
                    }
                })();
            });
        }

        // Wire tiles
        document.querySelectorAll('[data-open-modal="upload-photos-modal"]').forEach(el => {
            el.addEventListener('click', () => {
                if (!photoUploading) {
                    resetPhotosModal();
                } else {
                    syncRunningPhotoModalUI();
                }
                modal.style.display = 'flex';
            });
        });
    }

    // ── init ──────────────────────────────────────────────────────────────────

    function init() {
        // Fetch server-configured chunk size (falls back to module default of 10 MB).
        fetch('/api/upload-config', { credentials: 'same-origin' })
            .then(r => r.ok ? r.json() : null)
            .then(d => {
                if (d && d.chunkSizeMB > 0) chunkSizeMB = d.chunkSizeMB;
                if (d && d.maxUploadGB > 0) maxUploadGB = d.maxUploadGB;
            })
            .catch(() => {});

        initZipModal();
        initPhotosModal();
    }

    init();

    return { init };
})();
