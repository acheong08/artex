// Mock switch. Public variables are injected at build time; only variables prefixed
// with NEXT_PUBLIC_ are available in the browser.
// Set NEXT_PUBLIC_MOCK=1 on Vercel to use mock data site-wide without a backend.
export const MOCK = process.env.NEXT_PUBLIC_MOCK === "1";
