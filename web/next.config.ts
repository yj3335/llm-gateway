import type { NextConfig } from "next";

const nextConfig: NextConfig = {
  // The Docker image sets NEXT_OUTPUT=standalone for a minimal runtime bundle;
  // local `next start` (dev, Playwright) uses the default output.
  ...(process.env.NEXT_OUTPUT === "standalone" ? { output: "standalone" as const } : {}),
  reactStrictMode: true,
  poweredByHeader: false,
};

export default nextConfig;
