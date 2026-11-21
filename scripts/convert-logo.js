const fs = require('fs');
const path = require('path');
const ASSETS_DIR = path.join(__dirname, "..", "public", "assets");

// Sizes for different use cases
const sizes = [16, 32, 48, 64, 128, 192, 256, 512];

async function convertLogoToPng() {
    const inputFile = path.join(ASSETS_DIR, 'favicon.svg');
    const allSizes = [...sizes, 'favicon'];

    // Skip when logos already exist (e.g. pregenerated at Docker build time)
    if (allSizes.every(size => fs.existsSync(path.join(ASSETS_DIR, `logo-${size}.png`)))) {
        console.log('Logo PNGs already present, skipping conversion.');
        return;
    }

    // Lazy require so the runtime image can ship without sharp installed
    let sharp;
    try {
        sharp = require('sharp');
    } catch {
        console.warn('sharp not installed and logo PNGs missing - skipping logo generation.');
        return;
    }

    console.log("Generating logo files...");
    // Create assets directory if it doesn't exist
    if (!fs.existsSync(ASSETS_DIR)) {
        fs.mkdirSync(ASSETS_DIR, { recursive: true });
    }

    for (const size of sizes) {
        await sharp(inputFile)
            .resize(size, size)
            .png()
            .toFile(path.join(ASSETS_DIR, `logo-${size}.png`));

        console.log(`Created ${size}x${size} PNG`);
    }

    // Create favicon.ico size
    await sharp(inputFile)
        .resize(32, 32)
        .png()
        .toFile(path.join(ASSETS_DIR, 'favicon.png'));

    console.log('Conversion complete!');
}

module.exports = { convertLogoToPng };
