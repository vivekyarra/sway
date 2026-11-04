import type { NextConfig } from "next";

const isProd = process.env.NODE_ENV === "production";
const engine = process.env.ENGINE_URL ?? "http://localhost:8080";

// Production: a static export embedded in the Go binary (one deployable, same-origin API).
// Development: the Next dev server proxies /api to the Go engine.
const nextConfig: NextConfig = {
  output: isProd ? "export" : undefined,
  trailingSlash: true,
  images: { unoptimized: true },
  poweredByHeader: false,
  reactStrictMode: true,
  ...(isProd
    ? {}
    : {
        async rewrites() {
          return [
            { source: "/api/:path*", destination: `${engine}/api/:path*` },
            { source: "/metrics", destination: `${engine}/metrics` },
          ];
        },
      }),
};

export default nextConfig;
