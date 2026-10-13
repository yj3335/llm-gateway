const nextJest = require("next/jest");

const createJestConfig = nextJest({ dir: "./" });

/** @type {import('jest').Config} */
module.exports = createJestConfig({
  testEnvironment: "jsdom",
  setupFilesAfterEnv: ["<rootDir>/jest.setup.ts"],
  moduleNameMapper: { "^@/(.*)$": "<rootDir>/src/$1" },
  testMatch: ["<rootDir>/__tests__/**/*.test.{ts,tsx}"],
  collectCoverageFrom: ["src/**/*.{ts,tsx}", "!src/app/layout.tsx"],
});
