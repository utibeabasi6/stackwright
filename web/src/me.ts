import { createContext, useContext } from "react";
import type { GetMeResponse } from "./gen/stackwright/v1/auth_pb";

export const MeContext = createContext<GetMeResponse | undefined>(undefined);

export function useMe(): GetMeResponse {
  const me = useContext(MeContext);
  if (!me) throw new Error("useMe outside the signed-in app");
  return me;
}
