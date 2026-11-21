const fs = require("fs");
const path = require("path");
const PUBLIC_DIR = path.join(__dirname, "..", "public");
const ASSETS_DIR = path.join(PUBLIC_DIR, "assets");

function getFiles(dir, basePath = "/") {
  let fileList = [];
  const files = fs.readdirSync(dir);

  files.forEach((file) => {
    const filePath = path.join(dir, file);
    const fileUrl = path.join(basePath, file).replace(/\\/g, "/");

    if (fs.statSync(filePath).isDirectory()) {
      fileList = fileList.concat(getFiles(filePath, fileUrl));
    } else {
      fileList.push(fileUrl);
    }
  });

  return fileList;
}

function buildAssetManifest() {
  return getFiles(PUBLIC_DIR);
}

function buildPWAManifest(siteTitle) {
  return {
    name: siteTitle,
    short_name: siteTitle,
    description: "A stupidly simple todo list",
    start_url: "/",
    display: "standalone",
    background_color: "#ffffff",
    theme_color: "#000000",
    icons: [
      {
        src: "assets/logo-192.png",
        type: "image/png",
        sizes: "192x192"
      },
      {
        src: "assets/logo-512.png",
        type: "image/png",
        sizes: "512x512"
      }
    ],
    orientation: "any"
  };
}

// Best-effort file writes for backwards compatibility. The server serves
// both manifests from memory, so a read-only filesystem must not crash boot.
function writeManifests(siteTitle) {
  const assetManifest = buildAssetManifest();
  const pwaManifest = buildPWAManifest(siteTitle);

  try {
    fs.writeFileSync(path.join(ASSETS_DIR, "asset-manifest.json"), JSON.stringify(assetManifest, null, 2));
    fs.writeFileSync(path.join(ASSETS_DIR, "manifest.json"), JSON.stringify(pwaManifest, null, 2));
  } catch (error) {
    console.warn("Could not write manifest files (read-only assets dir?):", error.code);
  }

  return { assetManifest, pwaManifest };
}

module.exports = { buildAssetManifest, buildPWAManifest, writeManifests, generatePWAManifest: writeManifests };
