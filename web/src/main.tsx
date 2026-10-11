import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { App } from "./App";
import { tokenFromFragment } from "./api";
import "./styles/tokens.css";
import "./styles/app.css";
import "./styles/pages.css";

tokenFromFragment();
createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
