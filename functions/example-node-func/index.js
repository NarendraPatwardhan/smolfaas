/**
 * SmolFaaS Example Node.js Function
 *
 * A simple HTTP function demonstrating Node.js runtime support.
 */

const http = require('http');
const os = require('os');

const port = process.env.PORT || 8000;

const server = http.createServer((req, res) => {
    const url = new URL(req.url, `http://${req.headers.host}`);

    if (url.pathname === '/health') {
        res.writeHead(200, { 'Content-Type': 'text/plain' });
        res.end('OK');
        return;
    }

    if (url.pathname === '/' || url.pathname === '') {
        const response = {
            message: 'Hello from SmolFaaS Node.js function!',
            runtime: 'node',
            timestamp: new Date().toISOString(),
            hostname: os.hostname(),
            nodeVersion: process.version
        };

        res.writeHead(200, { 'Content-Type': 'application/json' });
        res.end(JSON.stringify(response, null, 2));
        return;
    }

    res.writeHead(404, { 'Content-Type': 'text/plain' });
    res.end('Not Found');
});

server.listen(port, '0.0.0.0', () => {
    console.log(`Node.js function starting on port ${port}...`);
});

// Graceful shutdown
process.on('SIGTERM', () => {
    console.log('Received SIGTERM, shutting down gracefully...');
    server.close(() => {
        console.log('Server closed');
        process.exit(0);
    });
});
