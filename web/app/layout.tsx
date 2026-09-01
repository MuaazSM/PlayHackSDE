import type { Metadata } from "next";
import "./globals.css";

export const metadata: Metadata = {
  title: "PlayHack | Sports bookings",
  description: "Book IIT Guwahati sports facilities with confidence.",
};

export default function RootLayout({ children }: Readonly<{ children: React.ReactNode }>) {
  return (
    <html lang="en">
      <body>{children}</body>
    </html>
  );
}
