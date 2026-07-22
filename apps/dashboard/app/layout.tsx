export const metadata = { title: "TheORM" };

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en">
      <body style={{ fontFamily: "ui-monospace, monospace", margin: 32 }}>{children}</body>
    </html>
  );
}
