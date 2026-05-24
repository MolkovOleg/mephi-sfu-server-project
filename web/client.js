// DOM Elements
const inputWsUrl = document.getElementById('input-ws-url');
const inputRoomId = document.getElementById('input-room-id');
const inputPeerId = document.getElementById('input-peer-id');
const btnConnect = document.getElementById('btn-connect');
const btnDisconnect = document.getElementById('btn-disconnect');

const toggleVideo = document.getElementById('toggle-video');
const toggleAudio = document.getElementById('toggle-audio');
const selectVideoDevice = document.getElementById('select-video-device');
const selectResolution = document.getElementById('select-resolution');

const localVideo = document.getElementById('local-video');
const localNoStream = document.getElementById('local-no-stream');
const videoGrid = document.getElementById('video-grid');

const wsStatusText = document.getElementById('ws-status-text');
const wsStatusDot = document.getElementById('ws-status-dot');
const rtcStatusText = document.getElementById('rtc-status-text');
const rtcStatusDot = document.getElementById('rtc-status-dot');

const statIceState = document.getElementById('stat-ice-state');
const statDtlsState = document.getElementById('stat-dtls-state');
const statBytesSent = document.getElementById('stat-bytes-sent');
const statBytesRecv = document.getElementById('stat-bytes-recv');

const logContainer = document.getElementById('log-container');
const btnClearLog = document.getElementById('btn-clear-log');
const logFilters = document.querySelectorAll('.log-filter');

// State
let ws = null;
let peerConnection = null;
let localStream = null;
let currentFilter = 'all';
let statsInterval = null;
let globalIceServers = [{ urls: 'stun:stun.l.google.com:19302' }];

// Helpers
function generateId() {
    return Math.random().toString(36).substring(2, 10);
}

function updateStatus(type, status, text) {
    const dot = type === 'ws' ? wsStatusDot : rtcStatusDot;
    const txt = type === 'ws' ? wsStatusText : rtcStatusText;

    dot.className = 'status-dot';
    if (status) dot.classList.add(status);
    txt.textContent = text;
}

function log(type, msg, obj = null) {
    const entry = document.createElement('div');
    entry.className = `log-entry ${type}`;
    if (currentFilter !== 'all' && currentFilter !== type) {
        entry.style.display = 'none';
    }

    const time = new Date().toLocaleTimeString('ru-RU', { hour12: false, hour: '2-digit', minute:'2-digit', second:'2-digit', fractionalSecondDigits: 3 });
    let text = `<span class="log-time">[${time}]</span> ${msg}`;
    if (obj) {
        text += `<br><pre style="margin-top:0.25rem;color:var(--text-muted)">${JSON.stringify(obj, null, 2)}</pre>`;
    }

    entry.innerHTML = text;
    logContainer.appendChild(entry);
    logContainer.scrollTop = logContainer.scrollHeight;
}

// Media
async function getMediaDevices() {
    try {
        const devices = await navigator.mediaDevices.enumerateDevices();
        const videoInputs = devices.filter(d => d.kind === 'videoinput');

        selectVideoDevice.innerHTML = '<option value="">По умолчанию</option>';
        videoInputs.forEach(device => {
            const option = document.createElement('option');
            option.value = device.deviceId;
            option.text = device.label || `Camera ${selectVideoDevice.length}`;
            selectVideoDevice.appendChild(option);
        });
    } catch (e) {
        log('error', 'Ошибка получения устройств', e.message);
    }
}

async function startLocalStream() {
    if (localStream) {
        localStream.getTracks().forEach(t => t.stop());
    }

    const useVideo = toggleVideo.checked;
    const useAudio = toggleAudio.checked;

    if (!useVideo && !useAudio) {
        localVideo.srcObject = null;
        localNoStream.style.display = 'flex';
        return;
    }

    const constraints = {
        audio: useAudio,
        video: false
    };

    if (useVideo) {
        const [width, height] = selectResolution.value.split('x').map(Number);
        constraints.video = {
            width: { ideal: width },
            height: { ideal: height }
        };
        if (selectVideoDevice.value) {
            constraints.video.deviceId = { exact: selectVideoDevice.value };
        }
    }

    try {
        log('media', 'Запрос доступа к медиа', constraints);
        localStream = await navigator.mediaDevices.getUserMedia(constraints);
        localVideo.srcObject = localStream;
        localNoStream.style.display = 'none';

        if (peerConnection) {
            const senders = peerConnection.getSenders();
            const newTracks = localStream.getTracks();

            // Фаза 1: Sender'ы без соответствующего нового трека → replaceTrack(null)
            // Это сигнализирует удалённой стороне что медиа выключено (надёжнее track.stop())
            senders.forEach(sender => {
                if (!sender.track) return;
                const stillActive = newTracks.find(t => t.kind === sender.track.kind);
                if (!stillActive) {
                    log('media', `Видео выключено: sender.replaceTrack(null) kind=${sender.track.kind}`);
                    sender.replaceTrack(null);
                }
            });

            // Фаза 2: Заменяем/добавляем треки которые есть в новом стриме
            newTracks.forEach(track => {
                const sender = senders.find(s => s.track && s.track.kind === track.kind);
                if (sender) {
                    sender.replaceTrack(track);
                } else {
                    peerConnection.addTrack(track, localStream);
                    log('media', `Добавлен трек в PC: ${track.kind}`);
                }
            });
        }

    } catch (e) {
        log('error', 'Ошибка доступа к медиа', e.message);
        localVideo.srcObject = null;
        localNoStream.style.display = 'flex';
        toggleVideo.checked = false;
        toggleAudio.checked = false;
    }
}

// Stats
function startStats() {
    if (statsInterval) clearInterval(statsInterval);
    statsInterval = setInterval(async () => {
        if (!peerConnection) return;
        try {
            const stats = await peerConnection.getStats();
            let bytesSent = 0;
            let bytesRecv = 0;

            stats.forEach(report => {
                if (report.type === 'outbound-rtp') bytesSent += report.bytesSent || 0;
                if (report.type === 'inbound-rtp') bytesRecv += report.bytesReceived || 0;
            });

            statBytesSent.textContent = (bytesSent / 1024).toFixed(1) + ' KB';
            statBytesRecv.textContent = (bytesRecv / 1024).toFixed(1) + ' KB';
        } catch (e) {}
    }, 1000);
}

// WebRTC
function createPeerConnection() {
    const config = {
        iceServers: globalIceServers
    };

    peerConnection = new RTCPeerConnection(config);
    log('rtc', 'Создан RTCPeerConnection');

    peerConnection.onicecandidate = (event) => {
        if (event.candidate) {
            log('rtc', 'Локальный ICE кандидат отправлен');
            sendWsMessage('candidate', {
                candidate: event.candidate.candidate,
                sdpMid: event.candidate.sdpMid,
                sdpMLineIndex: event.candidate.sdpMLineIndex,
                usernameFragment: event.candidate.usernameFragment
            });
        }
    };

    peerConnection.oniceconnectionstatechange = () => {
        statIceState.textContent = peerConnection.iceConnectionState;
        log('rtc', `ICE состояние изменено: ${peerConnection.iceConnectionState}`);
        if (peerConnection.iceConnectionState === 'connected' || peerConnection.iceConnectionState === 'completed') {
            updateStatus('rtc', 'connected', 'WebRTC: Подключено');
        } else if (peerConnection.iceConnectionState === 'disconnected' || peerConnection.iceConnectionState === 'failed') {
            updateStatus('rtc', 'disconnected', 'WebRTC: Ошибка/Отключено');
        }
    };

    // Обновляем DTLS/общее состояние соединения
    peerConnection.onconnectionstatechange = () => {
        const state = peerConnection.connectionState;
        statDtlsState.textContent = state;
        log('rtc', `Состояние соединения: ${state}`);
    };

    peerConnection.ontrack = (event) => {
        log('media', `Получен удаленный трек: ${event.track.kind} от ${event.streams[0]?.id || 'unknown'}`);

        const track = event.track;
        const stream = event.streams[0];
        if (!stream) return;

        let tile = document.getElementById(`tile-${stream.id}`);
        let videoEl, noStreamEl;

        if (!tile) {
            tile = document.createElement('div');
            tile.className = 'video-tile';
            tile.id = `tile-${stream.id}`;

            videoEl = document.createElement('video');
            videoEl.autoplay = true;
            videoEl.playsInline = true;
            videoEl.srcObject = stream;

            // Плейсхолдер «нет видео» — показывается когда трек завершён/заглушён
            noStreamEl = document.createElement('div');
            noStreamEl.className = 'video-no-stream';
            noStreamEl.style.display = 'none';
            noStreamEl.innerHTML = `
                <svg width="48" height="48" viewBox="0 0 48 48" fill="none"
                     stroke="currentColor" stroke-width="1.5">
                    <rect x="6" y="10" width="36" height="24" rx="3"/>
                    <polygon points="42,16 48,12 48,34 42,30"/>
                    <line x1="6" y1="10" x2="42" y2="34" stroke-width="2"/>
                </svg>
                <span>Нет видео</span>`;

            const label = document.createElement('div');
            label.className = 'video-label';
            label.innerHTML = `<span class="video-label-dot" style="background-color:var(--success)"></span>Удалённый: ${stream.id.substring(0, 6)}...`;

            tile.appendChild(videoEl);
            tile.appendChild(noStreamEl);
            tile.appendChild(label);
            videoGrid.appendChild(tile);

            // Плитка исчезает когда ВСЕ треки стрима удалены
            stream.onremovetrack = () => {
                if (stream.getTracks().filter(t => t.readyState === 'live').length === 0) {
                    tile.remove();
                    log('media', `Плитка удалена: стрим ${stream.id.substring(0, 6)}`);
                }
            };
        } else {
            videoEl = tile.querySelector('video');
            noStreamEl = tile.querySelector('.video-no-stream');
        }

        // ── Обработчики жизненного цикла трека ──────────────────────────────
        // ended: пир отключился или трек удалён при renegotiation
        track.onended = () => {
            log('media', `Трек завершён (ended): ${track.kind}`);
            // Если видеотрек завершён — показываем плейсхолдер
            if (track.kind === 'video' && noStreamEl) {
                noStreamEl.style.display = 'flex';
            }
            // Удаляем плитку если все live-треки стрима завершились
            const liveTracks = stream.getTracks().filter(t => t.readyState === 'live');
            if (liveTracks.length === 0) {
                setTimeout(() => tile.remove(), 300); // небольшая задержка для анимации
            }
        };

        // mute: временная потеря данных (сеть/пауза)
        track.onmute = () => {
            log('media', `Трек заглушён: ${track.kind}`);
            if (track.kind === 'video' && noStreamEl) {
                noStreamEl.style.display = 'flex';
            }
        };

        // unmute: трек восстановился
        track.onunmute = () => {
            log('media', `Трек восстановлен: ${track.kind}`);
            if (track.kind === 'video' && noStreamEl) {
                noStreamEl.style.display = 'none';
            }
        };
    };

    // Add local tracks
    if (localStream) {
        localStream.getTracks().forEach(track => {
            peerConnection.addTrack(track, localStream);
            log('media', `Добавлен локальный трек: ${track.kind}`);
        });
    }

    startStats();
}

async function handleOffer(payload) {
    if (!peerConnection) createPeerConnection();

    log('rtc', 'Получен SDP Offer от сервера');

    try {
        await peerConnection.setRemoteDescription(new RTCSessionDescription({
            type: payload.type,
            sdp: payload.sdp
        }));

        // Убеждаемся, что локальные треки добавлены в PC перед созданием Answer.
        // Это нужно если: a) localStream появился после createPeerConnection,
        // или b) первый offer пришёл быстрее чем стартанул startLocalStream.
        if (localStream) {
            const senders = peerConnection.getSenders();
            localStream.getTracks().forEach(track => {
                const alreadyAdded = senders.find(s => s.track && s.track.kind === track.kind);
                if (!alreadyAdded) {
                    peerConnection.addTrack(track, localStream);
                    log('media', `Трек добавлен в PC перед Answer: ${track.kind}`);
                }
            });
        }

        const answer = await peerConnection.createAnswer();
        await peerConnection.setLocalDescription(answer);

        log('rtc', 'Отправлен SDP Answer серверу');
        sendWsMessage('answer', {
            type: answer.type,
            sdp: answer.sdp
        });

    } catch (e) {
        log('error', 'Ошибка обработки Offer', e.message);
    }
}

async function handleCandidate(payload) {
    if (!peerConnection) return;
    try {
        await peerConnection.addIceCandidate(new RTCIceCandidate({
            candidate: payload.candidate,
            sdpMid: payload.sdpMid,
            sdpMLineIndex: payload.sdpMLineIndex,
            usernameFragment: payload.usernameFragment
        }));
        log('rtc', 'Добавлен удаленный ICE кандидат');
    } catch (e) {
        log('error', 'Ошибка добавления ICE кандидата', e.message);
    }
}

// WebSocket
function connectWs() {
    const url = inputWsUrl.value.trim();
    if (!url) return;

    updateStatus('ws', 'connecting', 'Подключение...');
    btnConnect.disabled = true;

    ws = new WebSocket(url);

    ws.onopen = () => {
        updateStatus('ws', 'connected', 'Подключено');
        btnDisconnect.disabled = false;

        let peerId = inputPeerId.value.trim();
        if (!peerId) {
            peerId = generateId();
            inputPeerId.value = peerId;
        }

        const roomId = inputRoomId.value.trim() || 'test-room';

        // Динамически строим HTTP URL для запроса ICE-серверов на основе адреса сокета
        let httpUrl;
        try {
            httpUrl = new URL(url);
            httpUrl.protocol = httpUrl.protocol === 'wss:' ? 'https:' : 'http:';
            httpUrl.pathname = '/ice-servers';
            httpUrl.searchParams.set('peer_id', peerId);
        } catch (e) {
            // Фолбэк на текущий хост
            httpUrl = new URL('/ice-servers', window.location.origin);
            httpUrl.searchParams.set('peer_id', peerId);
        }

        log('ws', `Запрос реквизитов STUN/TURN: ${httpUrl.toString()}`);
        fetch(httpUrl.toString())
            .then(res => res.json())
            .then(servers => {
                globalIceServers = servers;
                log('rtc', 'Успешно настроены STUN/TURN серверы', servers);
            })
            .catch(err => {
                log('error', 'Не удалось получить ICE конфигурацию, используем дефолтную', err.message);
                globalIceServers = [{ urls: 'stun:stun.l.google.com:19302' }];
            })
            .finally(() => {
                log('ws', `Отправка запроса join (room: ${roomId}, peer: ${peerId})`);
                sendWsMessage('join', {
                    room_id: roomId,
                    peer_id: peerId
                });
            });
    };

    ws.onmessage = (event) => {
        try {
            const msg = JSON.parse(event.data);
            log('ws', `Получено: ${msg.type}`, msg.payload);

            switch (msg.type) {
                case 'offer':
                    handleOffer(msg.payload);
                    break;
                case 'candidate':
                    handleCandidate(msg.payload);
                    break;
                case 'error':
                    log('error', `Ошибка от сервера: ${msg.payload.message} (${msg.payload.code})`);
                    break;
                default:
                    log('ws', `Неизвестный тип сообщения: ${msg.type}`);
            }
        } catch (e) {
            log('error', 'Ошибка парсинга WS сообщения', event.data);
        }
    };

    ws.onclose = () => {
        updateStatus('ws', 'disconnected', 'Отключено');
        updateStatus('rtc', 'disconnected', 'WebRTC: —');
        btnConnect.disabled = false;
        btnDisconnect.disabled = true;

        if (peerConnection) {
            peerConnection.close();
            peerConnection = null;
        }

        // Remove all remote tiles
        document.querySelectorAll('.video-tile:not(#tile-local)').forEach(el => el.remove());

        if (statsInterval) clearInterval(statsInterval);

        statIceState.textContent = '—';
        statBytesSent.textContent = '0';
        statBytesRecv.textContent = '0';

        log('ws', 'WebSocket соединение закрыто');
    };

    ws.onerror = (e) => {
        log('error', 'WebSocket ошибка');
    };
}

function disconnectWs() {
    if (ws) {
        ws.close();
    }
}

function sendWsMessage(type, payload) {
    if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type, payload }));
    }
}

// Event Listeners
btnConnect.addEventListener('click', connectWs);
btnDisconnect.addEventListener('click', disconnectWs);

toggleVideo.addEventListener('change', startLocalStream);
toggleAudio.addEventListener('change', startLocalStream);
selectVideoDevice.addEventListener('change', startLocalStream);
selectResolution.addEventListener('change', startLocalStream);

btnClearLog.addEventListener('click', () => {
    logContainer.innerHTML = '';
});

logFilters.forEach(btn => {
    btn.addEventListener('click', () => {
        logFilters.forEach(b => b.classList.remove('active'));
        btn.classList.add('active');
        currentFilter = btn.dataset.filter;

        const entries = document.querySelectorAll('.log-entry');
        entries.forEach(entry => {
            if (currentFilter === 'all' || entry.classList.contains(currentFilter)) {
                entry.style.display = 'block';
            } else {
                entry.style.display = 'none';
            }
        });
    });
});

// Init
inputPeerId.value = generateId();
navigator.mediaDevices.getUserMedia({ audio: true, video: true })
    .then(() => getMediaDevices().then(startLocalStream))
    .catch(e => log('error', 'Изначальная ошибка доступа к медиа', e.message));
