import { fileURLToPath } from "node:url";

// Static export: `NEXT_EXPORT=1 next build` creates a static directory in web/out that
// can be served directly from an nginx web root. Leave this unset for next dev to keep
// /api proxying and hot reload.
const isExport = process.env.NEXT_EXPORT === "1";
// Vercel demo: the entire site uses mocks, with no backend or /api proxy.
const isMock = process.env.NEXT_PUBLIC_MOCK === "1";

/** @type {import('next').NextConfig} */
const nextConfig = {
  // Prevent parent-directory lockfiles from affecting root detection and asset paths.
  turbopack: { root: fileURLToPath(new URL(".", import.meta.url)) },
  reactCompiler: true,
  // Allow access to dev assets (HMR) from LAN IPs; adjust as needed.
  // During development, allow /_next/* and HMR from any IPv4 source, even if the LAN IP changes.
  // Next disallows a bare "*" for security; use segmented wildcards. "*.*.*.*" matches any IPv4 address.
  allowedDevOrigins: ["*.*.*.*"],
  compiler: {
    removeConsole: process.env.NODE_ENV === "production",
  },
  ...(isExport
    ? {
        // Pure static export: no Node runtime, images are not optimized, and each route generates <route>/index.html.
        output: "export",
        images: { unoptimized: true },
        trailingSlash: true,
      }
    : isMock
      ? {
          // Vercel mock demo: no backend or /api proxy is needed.
          images: { unoptimized: true },
        }
      : {
          // Development: proxy /api/* to the Go backend (default :8787; override with AUTOPENTEST_API).
          async rewrites() {
            const backend = process.env.AUTOPENTEST_API ?? "http://localhost:8787";
            return [{ source: "/api/:path*", destination: `${backend}/api/:path*` }];
          },
        }),
};

export default nextConfig;
