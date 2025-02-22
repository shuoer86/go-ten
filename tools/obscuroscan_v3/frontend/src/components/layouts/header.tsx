import { MainNav } from "../main-nav";
import { ModeToggle } from "../mode-toggle";
import ConnectWalletButton from "../modules/common/connect-wallet";
import Link from "next/link";
import { HamburgerMenuIcon } from "@radix-ui/react-icons";
import { useState } from "react";
import { Button } from "../ui/button";

export default function Header() {
  return (
    <div className="border-b">
      <div className="flex h-16 justify-between items-center px-4">
        <Link href="/">
          <h1 className="text-40">TEN.</h1>
        </Link>
        <div className="hidden md:flex items-center space-x-4">
          <MainNav className="mx-6" />
          <div className="flex items-center space-x-4">
            <ModeToggle />
            <ConnectWalletButton />
          </div>
        </div>
        <div className="flex items-center space-x-4 md:hidden">
          <MobileMenu />
        </div>
      </div>
    </div>
  );
}

const MobileMenu = () => {
  const [menuOpen, setMenuOpen] = useState(false);

  return (
    <div className="relative">
      <ModeToggle />
      <Button
        variant={"clear"}
        className="text-muted-foreground hover:text-primary transition-colors"
        onClick={() => setMenuOpen(!menuOpen)}
      >
        <HamburgerMenuIcon />
      </Button>
      {menuOpen && (
        <div className="absolute z-10 top-0 right-0 mt-12">
          <div className="bg-background border rounded-lg shadow-lg">
            <div className="flex flex-col p-4 space-y-2">
              <MainNav className="flex flex-col" />
              <ConnectWalletButton />
            </div>
          </div>
        </div>
      )}
    </div>
  );
};
