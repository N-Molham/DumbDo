// Bun auto-loads .env files; dotenv is no longer needed
const express = require('express');
const cors = require('cors');
const fs = require('fs').promises;
const path = require('path');
const crypto = require('crypto');
const cookieParser = require('cookie-parser');
const app = express();
app.disable('x-powered-by');
const { getCorsOptions, originValidationMiddleware } = require('./scripts/cors');
const { convertLogoToPng } = require('./scripts/convert-logo');
const { writeManifests } = require('./scripts/pwa-manifest-generator');

// Environment variables
const PORT = process.env.PORT || 3000;
const PIN = process.env.DUMBDO_PIN;
const SITE_TITLE = process.env.DUMBDO_SITE_TITLE || 'DumbDo';
const SINGLE_LIST = process.env.SINGLE_LIST === 'true' || false;
const MIN_PIN_LENGTH = 4;
const MAX_PIN_LENGTH = 10;
const PUBLIC_DIR = path.join(__dirname, 'public');
const ASSETS_DIR = path.join(PUBLIC_DIR, 'assets');

// Generate PWA assets and keep both manifests in memory (served from routes,
// so a read-only assets directory never breaks boot)
const { assetManifest, pwaManifest } = writeManifests(SITE_TITLE);
convertLogoToPng();

// Trust proxy - required for secure cookies behind a reverse proxy
app.set('trust proxy', 1);

// Cors Setup
const corsOptions = getCorsOptions();

// Middleware
app.use(cors(corsOptions));
app.use(express.json());
app.use(cookieParser());

// Apply origin validation to all /api routes
app.use('/api', originValidationMiddleware);

// Brute force protection
const loginAttempts = new Map();  // Stores IP addresses and their attempt counts
const MAX_ATTEMPTS = 5;           // Maximum allowed attempts
const LOCKOUT_TIME = 15 * 60 * 1000; // 15 minutes in milliseconds

// Reset attempts for an IP
function resetAttempts(ip) {
    loginAttempts.delete(ip);
}

// Check if an IP is locked out
function isLockedOut(ip) {
    const attempts = loginAttempts.get(ip);
    if (!attempts) return false;
    
    if (attempts.count >= MAX_ATTEMPTS) {
        const timeElapsed = Date.now() - attempts.lastAttempt;
        if (timeElapsed < LOCKOUT_TIME) {
            return true;
        }
        resetAttempts(ip);
    }
    return false;
}

// Record an attempt for an IP
function recordAttempt(ip) {
    const attempts = loginAttempts.get(ip) || { count: 0, lastAttempt: 0 };
    attempts.count += 1;
    attempts.lastAttempt = Date.now();
    loginAttempts.set(ip, attempts);
}

// Cleanup old lockouts periodically
setInterval(() => {
    const now = Date.now();
    for (const [ip, attempts] of loginAttempts.entries()) {
        if (now - attempts.lastAttempt >= LOCKOUT_TIME) {
            loginAttempts.delete(ip);
        }
    }
}, 60000); // Clean up every minute
const loginAttemptsCleanup = setInterval(() => {
    const now = Date.now();
    for (const [ip, attempts] of loginAttempts.entries()) {
        if (now - attempts.lastAttempt >= LOCKOUT_TIME) {
            loginAttempts.delete(ip);
        }
    }
}, 60000); // Clean up every minute
loginAttemptsCleanup.unref();

// Constant-time string comparison
function secureCompare(a, b) {
    if (typeof a !== 'string' || typeof b !== 'string') {
        return false;
    }
    
    return crypto.timingSafeEqual(
        Buffer.from(a.padEnd(MAX_PIN_LENGTH, '0')), 
        Buffer.from(b.padEnd(MAX_PIN_LENGTH, '0'))
    );
}

// Public PIN Routes - these don't require authentication
app.get('/api/pin-required', (req, res) => {
    const lockoutTime = isLockedOut(req.ip);
    const attempts = loginAttempts.get(req.ip);
    const attemptsLeft = attempts ? MAX_ATTEMPTS - attempts.count : MAX_ATTEMPTS;
    
    res.json({ 
        required: !!PIN,
        length: PIN ? PIN.length : MIN_PIN_LENGTH,
        locked: isLockedOut(req.ip),
        attemptsLeft: Math.max(0, attemptsLeft),
        lockoutMinutes: lockoutTime ? Math.ceil((LOCKOUT_TIME - (Date.now() - attempts.lastAttempt)) / 1000 / 60) : 0
    });
});

app.post('/api/verify-pin', (req, res) => {
    const { pin } = req.body;
    const ip = req.ip;
    
    // Check if IP is locked out
    if (isLockedOut(ip)) {
        const attempts = loginAttempts.get(ip);
        const timeLeft = Math.ceil((LOCKOUT_TIME - (Date.now() - attempts.lastAttempt)) / 1000 / 60);
        return res.status(429).json({ 
            error: `Too many attempts. Please try again in ${timeLeft} minutes.`,
            locked: true,
            lockoutMinutes: timeLeft
        });
    }
    
    // Validate PIN length
    if (PIN && (pin.length < MIN_PIN_LENGTH || pin.length > MAX_PIN_LENGTH)) {
        recordAttempt(ip);
        const attempts = loginAttempts.get(ip);
        return res.status(401).json({ 
            valid: false,
            error: `PIN must be between ${MIN_PIN_LENGTH} and ${MAX_PIN_LENGTH} digits`,
            attemptsLeft: MAX_ATTEMPTS - attempts.count
        });
    }
    
    // Add artificial delay to further prevent timing attacks
    const delay = crypto.randomInt(50, 150);
    setTimeout(() => {
        if (!PIN || secureCompare(pin, PIN)) {
            // Reset attempts on successful login
            resetAttempts(ip);
            
            // Set secure cookie
            res.cookie('DUMBDO_PIN', pin, {
                httpOnly: true,
                secure: process.env.NODE_ENV === 'production',
                sameSite: 'strict'
            });
            
            res.json({ valid: true });
        } else {
            // Record failed attempt
            recordAttempt(ip);
            
            const attempts = loginAttempts.get(ip);
            const attemptsLeft = MAX_ATTEMPTS - attempts.count;
            
            res.status(401).json({ 
                valid: false,
                error: `Invalid PIN. ${attemptsLeft} attempts remaining before lockout.`,
                attemptsLeft
            });
        }
    }, delay);
});

// Get site configuration
app.get('/api/config', (req, res) => {
    res.json({
        siteTitle: SITE_TITLE,
        singleList: SINGLE_LIST
    });
});

// Serve static files that don't need PIN protection
app.get('/login.js', (req, res) => {
    res.sendFile(path.join(PUBLIC_DIR, 'login.js'));
});

app.get('/styles.css', (req, res) => {
    res.sendFile(path.join(ASSETS_DIR, 'styles.css'));
});

app.get('/favicon.svg', (req, res) => {
    res.sendFile(path.join(ASSETS_DIR, 'favicon.svg'));
});

// Serve the pwa/asset manifest (kept in memory at boot)
app.get('/asset-manifest.json', (req, res) => {
    res.json(assetManifest);
});
app.get('/manifest.json', (req, res) => {
    res.json(pwaManifest);
});

// PIN validation helper
function isValidPin(providedPin) {
    return !PIN || (providedPin && secureCompare(providedPin, PIN));
}

// PIN validation middleware - everything after this requires PIN
app.use((req, res, next) => {
    const providedPin = req.cookies.DUMBDO_PIN || req.headers['x-pin'];
    
    if (isValidPin(providedPin)) {
        return next();
    }

    if (req.xhr || req.path.startsWith('/api/')) {
        return res.status(401).json({ error: 'Invalid PIN' });
    }
    
    if (req.path !== '/login') {
        return res.redirect('/login');
    }
    
    next();
});

// Public health endpoint for container healthchecks (no PIN required)
app.get('/health', (req, res) => {
    res.json({ ok: true });
});

// Protected routes below

app.get('/', (req, res) => {
    res.sendFile(path.join(PUBLIC_DIR, 'index.html'));
});

app.get('/login', (req, res) => {
    const providedPin = req.cookies.DUMBDO_PIN || req.headers['x-pin'];
    
    if (isValidPin(providedPin)) {
        res.redirect('/');
    } else {
        res.sendFile(path.join(PUBLIC_DIR, 'login.html'));
    }
});

// Protect all other static files
// NOTE: never serve the project root ('.') — it leaked server.js, package.json and node_modules
app.use(express.static(PUBLIC_DIR, { maxAge: '1h' }));

// Data directory and file path
const DATA_DIR = path.join(__dirname, 'data');
const DATA_FILE = path.join(DATA_DIR, 'todos.json');

// In-memory cache: reads served from RAM, disk only touched on writes
let todosCache = null;

// Ensure the data directory and file exist
async function initDataFile() {
    try {
        await fs.access(DATA_DIR);
    } catch {
        await fs.mkdir(DATA_DIR, { recursive: true });
    }

    try {
        const data = await fs.readFile(DATA_FILE, 'utf8');
        todosCache = JSON.parse(data);
    } catch {
        todosCache = {};
        await atomicWriteTodos(todosCache);
    }

    console.log('Todo list stored at:', DATA_FILE);
}

// Atomic write: temp file + rename, so a crash can never leave a half-written todos.json
async function atomicWriteTodos(data) {
    const tmp = DATA_FILE + '.tmp';
    await fs.writeFile(tmp, JSON.stringify(data, null, 2));
    await fs.rename(tmp, DATA_FILE);
}

// Protected API routes
app.get('/api/todos', async (req, res) => {
    if (todosCache === null) {
        try {
            todosCache = JSON.parse(await fs.readFile(DATA_FILE, 'utf8'));
        } catch (error) {
            return res.status(500).json({ error: 'Failed to read todos' });
        }
    }
    res.json(todosCache);
});

app.post('/api/todos', async (req, res) => {
    if (typeof req.body !== 'object' || req.body === null || Array.isArray(req.body)) {
        return res.status(400).json({ error: 'Invalid todos payload' });
    }
    try {
        todosCache = req.body;
        await atomicWriteTodos(todosCache);
        res.json({ success: true });
    } catch (error) {
        res.status(500).json({ error: 'Failed to save todos' });
    }
});

// Initialize and start server
initDataFile().then(() => {
    app.listen(PORT, () => {
        console.log(`DumbDo server running at http://localhost:${PORT}`);
        console.log('PIN protection:', PIN ? 'enabled' : 'disabled');
    });
}); 