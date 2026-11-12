import { useEffect, useRef, useState } from "react";
import { message } from "./api";

// useLoad returns nothing for a new key until that key loads, never the data
// of the key before it. A reload of the same key keeps its data.
export function useLoad<T>(key: string, load: () => Promise<T>) {
  const [loaded, setLoaded] = useState<{ key: string; data?: T; error?: string }>({ key });
  const [version, setVersion] = useState(0);
  const loader = useRef(load);
  loader.current = load;
  useEffect(() => {
    let live = true;
    loader.current().then(
      (data) => {
        if (live) setLoaded({ key, data });
      },
      (e) => {
        if (live) setLoaded((prev) => ({ key, data: prev.key === key ? prev.data : undefined, error: message(e) }));
      },
    );
    return () => {
      live = false;
    };
  }, [key, version]);
  const current = loaded.key === key;
  return {
    data: current ? loaded.data : undefined,
    error: current ? loaded.error : undefined,
    reload: () => setVersion((v) => v + 1),
  };
}
