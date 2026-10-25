const http = require('http');
const fs = require('fs');
const crypto = require('crypto');
const { execSync } = require('child_process');

const LISTEN_PORT = 8888;
const LISTEN_HOST = '0.0.0.0';
const CREDENTIALS_FILE = 'C:\\Users\\Administrator\\.dsh\\.credentials.yaml';

// Prevent any unexpected process crashes
process.on('uncaughtException', (err) => {
  console.error(`[${new Date().toLocaleTimeString()}] [DSH Proxy UncaughtException]:`, err.message);
});
process.on('unhandledRejection', (reason) => {
  console.error(`[${new Date().toLocaleTimeString()}] [DSH Proxy UnhandledRejection]:`, reason);
});

function decodeBase64Url(value) {
  const padding = '='.repeat((4 - value.length % 4) % 4);
  return Buffer.from(value.replaceAll('-', '+').replaceAll('_', '/') + padding, 'base64');
}

function encodeBase64Url(value) {
  return Buffer.from(value).toString('base64').replaceAll('+', '-').replaceAll('/', '_').replace(/=+$/u, '');
}

function getSecret() {
  try {
    const content = fs.readFileSync(CREDENTIALS_FILE, 'utf8');
    const match = content.match(/client-connection\/browser-session:[\s\S]*?secret:\s*([A-Za-z0-9_-]+)/);
    if (match && match[1]) {
      return decodeBase64Url(match[1]);
    }
  } catch (err) {
    console.error('[DSH Proxy] Error reading credentials:', err.message);
  }
  return null;
}

// Probe a candidate port to verify it is genuinely DSH Web GUI (and not internal plugin ports like 8790)
function probePort(port) {
  return new Promise((resolve) => {
    const req = http.request({
      hostname: '127.0.0.1',
      port: port,
      path: '/',
      method: 'GET',
      timeout: 800
    }, (res) => {
      let data = '';
      res.on('data', chunk => {
        data += chunk;
        if (data.length > 2048) res.destroy(); // read enough to verify
      });
      res.on('end', () => {
        if (data.includes('dsh web authentication required') || data.includes('window.__DSH_BOOT__') || data.includes('DeepSeek Harness')) {
          resolve(true);
        } else {
          resolve(false);
        }
      });
    });
    req.on('error', () => resolve(false));
    req.on('timeout', () => {
      req.destroy();
      resolve(false);
    });
    req.end();
  });
}

function getCandidatePorts() {
  const candidates = [];
  try {
    const tasklist = execSync('tasklist /FI "IMAGENAME eq DeepSeek Harness.exe" /FO CSV /NH', { encoding: 'utf8' });
    const pids = new Set();
    for (const line of tasklist.split(/\r?\n/)) {
      const parts = line.split(',');
      if (parts.length >= 2) {
        const clean = parts[1].replace(/["\s]/g, '');
        const pidNum = parseInt(clean, 10);
        if (!isNaN(pidNum)) pids.add(pidNum);
      }
    }
    if (pids.size === 0) return candidates;

    const netstat = execSync('netstat -ano', { encoding: 'utf8' });
    for (const line of netstat.split(/\r?\n/)) {
      if (line.includes('127.0.0.1:') && line.includes('LISTENING')) {
        const cols = line.trim().split(/\s+/);
        const addr = cols[1];
        const pid = parseInt(cols[cols.length - 1], 10);
        if (pids.has(pid)) {
          const m = addr.match(/127\.0\.0\.1:(\d+)/);
          if (m) {
            const port = parseInt(m[1], 10);
            if (!candidates.includes(port)) {
              candidates.push(port);
            }
          }
        }
      }
    }
  } catch (err) {
    console.error('[DSH Proxy] Error collecting candidate ports:', err.message);
  }
  return candidates;
}

let cachedDshPort = 64938;

async function refreshRealPort() {
  const ports = getCandidatePorts();
  for (const p of ports) {
    const isRealWeb = await probePort(p);
    if (isRealWeb) {
      if (p !== cachedDshPort) {
        console.log(`[${new Date().toLocaleTimeString()}] [DSH Proxy] Switch active backend port -> 127.0.0.1:${p}`);
        cachedDshPort = p;
      }
      return p;
    }
  }
  return cachedDshPort;
}

// Initial detection
refreshRealPort().then(port => {
  console.log(`[${new Date().toLocaleTimeString()}] [DSH Proxy] Validated active Web GUI Port: 127.0.0.1:${port}`);
});

// Periodic validation every 15 seconds
setInterval(refreshRealPort, 15000);

function generateAuthCookie(port) {
  const secret = getSecret();
  if (!secret) return null;
  const authority = `127.0.0.1:${port}`;
  const cookieName = 'dsh-auth-' + encodeBase64Url(crypto.createHash('sha256').update(authority).digest());
  const issuedAt = Date.now();
  const expiresAt = issuedAt + 24 * 60 * 60 * 1000;
  const payload = {
    version: 1,
    authority: authority,
    issuedAt: issuedAt,
    expiresAt: expiresAt
  };
  const body = encodeBase64Url(Buffer.from(JSON.stringify(payload), 'utf8'));
  const sig = crypto.createHmac('sha256', secret).update(body).digest();
  const cookieVal = `v1.${body}.${encodeBase64Url(sig)}`;
  return `${cookieName}=${cookieVal}`;
}

const server = http.createServer((req, res) => {
  const currentPort = cachedDshPort;
  const authority = `127.0.0.1:${currentPort}`;
  const authCookie = generateAuthCookie(currentPort);

  const reqHeaders = {
    ...req.headers,
    host: authority
  };

  if (authCookie) {
    reqHeaders.cookie = authCookie;
  }

  if (reqHeaders.origin) {
    reqHeaders.origin = `http://${authority}`;
  }

  const options = {
    hostname: '127.0.0.1',
    port: currentPort,
    path: req.url,
    method: req.method,
    headers: reqHeaders
  };

  const proxyReq = http.request(options, (proxyRes) => {
    res.writeHead(proxyRes.statusCode, proxyRes.headers);
    proxyRes.pipe(res, { end: true });
  });

  proxyReq.on('error', (err) => {
    console.error(`[${new Date().toLocaleTimeString()}] [Proxy Req Error]: ${err.message}`);
    // Trigger immediate re-detection
    refreshRealPort();
    res.writeHead(502, { 'Content-Type': 'text/plain; charset=utf-8' });
    res.end(`[DSH Proxy Error] Unable to connect to DSH Backend on 127.0.0.1:${currentPort}\n${err.message}`);
  });

  req.on('error', (err) => {
    console.error(`[${new Date().toLocaleTimeString()}] [Incoming Req Error]: ${err.message}`);
    proxyReq.destroy();
  });

  req.pipe(proxyReq, { end: true });
});

// Guard against bad client sockets
server.on('clientError', (err, socket) => {
  if (err.code === 'ECONNRESET' || !socket.writable) return;
  socket.end('HTTP/1.1 400 Bad Request\r\n\r\n');
});

// WebSocket support
server.on('upgrade', (req, socket, head) => {
  socket.on('error', (err) => {
    // Suppress unhandled socket error on sudden disconnects
  });

  const currentPort = cachedDshPort;
  const authority = `127.0.0.1:${currentPort}`;
  const authCookie = generateAuthCookie(currentPort);

  const reqHeaders = {
    ...req.headers,
    host: authority
  };

  if (authCookie) {
    reqHeaders.cookie = authCookie;
  }

  if (reqHeaders.origin) {
    reqHeaders.origin = `http://${authority}`;
  }

  const options = {
    hostname: '127.0.0.1',
    port: currentPort,
    path: req.url,
    method: req.method,
    headers: reqHeaders
  };

  const proxyReq = http.request(options);

  proxyReq.on('upgrade', (proxyRes, proxySocket, proxyHead) => {
    proxySocket.on('error', () => {
      socket.destroy();
    });

    socket.write(`HTTP/${proxyRes.httpVersion} ${proxyRes.statusCode} ${proxyRes.statusMessage}\r\n`);
    for (const [key, value] of Object.entries(proxyRes.headers)) {
      if (Array.isArray(value)) {
        for (const v of value) socket.write(`${key}: ${v}\r\n`);
      } else {
        socket.write(`${key}: ${value}\r\n`);
      }
    }
    socket.write('\r\n');

    if (proxyHead && proxyHead.length) socket.write(proxyHead);
    proxySocket.pipe(socket);
    socket.pipe(proxySocket);
  });

  proxyReq.on('error', (err) => {
    console.error(`[${new Date().toLocaleTimeString()}] [Proxy WS Error]: ${err.message}`);
    socket.destroy();
  });

  proxyReq.end();
});

server.listen(LISTEN_PORT, LISTEN_HOST, () => {
  console.log(`=================================================`);
  console.log(`  DeepSeek Harness 远程控制高可用代理已启动`);
  console.log(`  Tailscale 专网地址:  http://100.99.78.100:${LISTEN_PORT}`);
  console.log(`  局域网 Wi-Fi 地址:   http://192.168.31.51:${LISTEN_PORT}`);
  console.log(`  防崩溃守护机制:      已启用 (零崩溃异常拦截)`);
  console.log(`  智能端口探针:        已启用 (过滤8790等插件内部端口)`);
  console.log(`=================================================`);
});
