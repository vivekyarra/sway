import type { Metadata, Viewport } from "next";
import { Inter, Instrument_Serif } from "next/font/google";
import { JudgeTour } from "@/components/JudgeTour";
import { Providers } from "@/components/Providers";
import { TopBar } from "@/components/TopBar";
import "./globals.css";

const inter = Inter({ subsets: ["latin"], variable: "--font-inter", display: "swap" });
const serif = Instrument_Serif({ subsets: ["latin"], weight: "400", style: ["normal", "italic"], variable: "--font-serif", display: "swap" });

export const metadata: Metadata = {
  title: "Sway: you swayed the sale, you get paid",
  description:
    "When an AI shopping agent buys because of what a human wrote, Sway measures whose content changed the decision and pays them through PayPal, but only if the buyer keeps the product.",
  icons: {
    icon: "data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 32 32'%3E%3Crect width='32' height='32' rx='9' fill='%2317141f'/%3E%3Cpath d='M7 19c3-6 6-6 9 0s6 6 9 0' fill='none' stroke='%238f7bff' stroke-width='3' stroke-linecap='round'/%3E%3Ccircle cx='16' cy='11' r='2.4' fill='%23fff'/%3E%3C/svg%3E",
  },
};

export const viewport: Viewport = { themeColor: "#fbfaf7", width: "device-width", initialScale: 1 };

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en" className={`${inter.variable} ${serif.variable}`}>
      <body className="min-h-screen">
        <a href="#main" className="sr-only focus:not-sr-only focus:fixed focus:left-4 focus:top-3 focus:z-[60] focus:rounded-lg focus:bg-ink focus:px-3 focus:py-2 focus:text-white">
          Skip to content
        </a>
        <Providers>
          <TopBar />
          <main id="main">{children}</main>
          <JudgeTour />
        </Providers>
      </body>
    </html>
  );
}
